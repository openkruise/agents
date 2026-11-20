/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2b

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/tracing"
	"go.opentelemetry.io/otel/attribute"
)

func (sc *Controller) CreateSnapshot(r *http.Request) (web.ApiResponse[*models.Snapshot], *web.ApiError) {
	ctx := r.Context()
	sandboxID := r.PathValue("sandboxID")
	log := klog.FromContext(ctx)
	start := time.Now()
	request, parseErr := sc.parseCreateSnapshotRequest(r)
	if parseErr != nil {
		return web.ApiResponse[*models.Snapshot]{}, parseErr
	}
	log.Info("create snapshot request received", "request", request)
	sbx, apiErr := sc.getSandboxOfUser(ctx, sandboxID, liveSandboxStates)
	if apiErr != nil {
		return web.ApiResponse[*models.Snapshot]{}, apiErr
	}
	if state, reason := sbx.GetState(); state != v1alpha1.SandboxStateRunning {
		log.Info("cannot create snapshot: sandbox is not running", "state", state, "reason", reason)
		return web.ApiResponse[*models.Snapshot]{}, withSandboxResourceContext(&web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Sandbox %s is not running", sbx.GetSandboxID()),
		}, sbx)
	}
	ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanManagerCreateSnapshot)
	// Register EndSpan via defer with a stable operation-error variable so the
	// span is structurally guaranteed to close on every return path, keeping
	// this call site consistent with the other manager operations. Keep the
	// closure: a direct defer tracing.EndSpan(ctx, span, err) would evaluate
	// err while still nil and record every failure as success.
	var err error
	defer func() { tracing.EndSpan(ctx, span, err) }()
	// Record optional request extensions as span attributes when present.
	if request.Extensions.KeepRunning != nil {
		span.SetAttributes(attribute.Bool(tracing.AttrSnapshotKeepRunning, *request.Extensions.KeepRunning))
	}
	if request.Extensions.TTL != nil {
		span.SetAttributes(attribute.String(tracing.AttrSnapshotTTL, *request.Extensions.TTL))
	}
	snapshotName := ""
	if request.Name != nil {
		snapshotName = *request.Name
	}
	// Each creation keeps its own ID. Name lookup selects the newest visible
	// checkpoint separately, so another creation cannot invalidate this ID.
	checkpointID, err := sbx.CreateCheckpoint(ctx, infra.CreateCheckpointOptions{
		Alias:              snapshotName,
		KeepRunning:        request.Extensions.KeepRunning,
		TTL:                request.Extensions.TTL,
		PersistentContents: request.Extensions.PersistentContents,
		WaitSuccessTimeout: time.Duration(request.Extensions.WaitSuccessSeconds) * time.Second,
	})
	if err != nil {
		log.Error(err, "failed to create checkpoint")
		snapshotTotal.WithLabelValues(sbx.GetNamespace(), "failure").Inc()
		// The sandbox backend rejects checkpoint aliases its storage cannot
		// hold (label-value rules) with ErrorBadRequest; surface that as 400.
		code := http.StatusInternalServerError
		message := "Failed to create snapshot"
		if managererrors.GetErrCode(err) == managererrors.ErrorBadRequest {
			code = http.StatusBadRequest
			message = "Invalid snapshot name"
		}
		return web.ApiResponse[*models.Snapshot]{}, withSandboxResourceContext(&web.ApiError{
			Code:    code,
			Message: message,
		}, sbx)
	}
	snapshotDuration.WithLabelValues(sbx.GetNamespace()).Observe(time.Since(start).Seconds())
	snapshotTotal.WithLabelValues(sbx.GetNamespace(), "success").Inc()
	names := []string{}
	if snapshotName != "" {
		names = []string{snapshotName}
	}
	return web.ApiResponse[*models.Snapshot]{
		Code: http.StatusCreated,
		Body: &models.Snapshot{
			SnapshotID: checkpointID,
			Names:      names,
		},
	}, nil
}

func (sc *Controller) parseCreateSnapshotRequest(r *http.Request) (models.NewSnapshotRequest, *web.ApiError) {
	reader := r.Body
	if reader == nil {
		reader = http.NoBody
	}
	// Decoding into a pointer leaves it nil for a JSON null, which is not an object.
	var body *models.NewSnapshotRequest
	if err := json.NewDecoder(reader).Decode(&body); err != nil || body == nil {
		message := "invalid snapshot request body: must be a JSON object"
		if errors.Is(err, io.EOF) {
			message = "snapshot request body is required"
		} else if err != nil {
			message = fmt.Sprintf("invalid snapshot request body: %s", err.Error())
		}
		return models.NewSnapshotRequest{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: message,
		}
	}
	request := *body
	if err := request.ParseExtensions(r.Header); err != nil {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Bad extension param: %s", err.Error()),
		}
	}
	// The persistent-contents header takes precedence: the body memory flag
	// only applies when no contents header is present. memory=true keeps memory
	// and filesystem, memory=false keeps filesystem only, and an omitted memory
	// keeps the sandbox default untouched.
	if len(request.Extensions.PersistentContents) == 0 && request.Memory != nil {
		if *request.Memory {
			request.Extensions.PersistentContents = []string{
				v1alpha1.CheckpointPersistentContentMemory,
				v1alpha1.CheckpointPersistentContentFilesystem,
			}
		} else {
			request.Extensions.PersistentContents = []string{v1alpha1.CheckpointPersistentContentFilesystem}
		}
	}
	return request, nil
}
