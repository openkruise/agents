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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
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

// snapshotAliasCleanupTimeout bounds the best-effort removal of superseded
// named snapshots after the replacement checkpoint has already succeeded.
const snapshotAliasCleanupTimeout = 5 * time.Second

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
	// A named snapshot supersedes the owner's Succeeded snapshot that already
	// carries the same alias (E2B: a new build is assigned to the existing
	// template instead of creating a new one). Reconciliation runs only after
	// the new checkpoint succeeded, so a failed creation never removes an
	// existing snapshot.
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
		if managererrors.GetErrCode(err) == managererrors.ErrorBadRequest {
			code = http.StatusBadRequest
		}
		return web.ApiResponse[*models.Snapshot]{}, withSandboxResourceContext(&web.ApiError{
			Code:    code,
			Message: err.Error(),
		}, sbx)
	}
	snapshotDuration.WithLabelValues(sbx.GetNamespace()).Observe(time.Since(start).Seconds())
	snapshotTotal.WithLabelValues(sbx.GetNamespace(), "success").Inc()
	// Reconcile the alias after the checkpoint exists: the newest holder wins
	// and every older holder is removed. A request that lost the race removes
	// its own checkpoint and reports the surviving holder.
	if snapshotName != "" {
		// The checkpoint records the sandbox owner, which getSandboxOfUser has
		// matched to the caller.
		owner := sbx.GetAnnotations()[v1alpha1.AnnotationOwner]
		checkpointID = sc.reconcileSnapshotAlias(ctx, sbx.GetNamespace(), owner, snapshotName, checkpointID)
	}
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

// reconcileSnapshotAlias keeps only the owner's newest Succeeded checkpoint
// carrying the alias and removes every older holder. It runs after the new
// checkpoint succeeded, so a failed creation never removes an existing
// snapshot, and it always reports the newest holder, which keeps the response
// usable when this request lost a concurrent race. Both the lookup and the
// deletion are scoped to the owner, matching snapshot list visibility and
// delete authorization. Cleanup is best effort and never fails the request.
func (sc *Controller) reconcileSnapshotAlias(ctx context.Context, namespace, owner, alias, createdCheckpointID string) string {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotAliasCleanupTimeout)
	defer cancel()
	log := klog.FromContext(cleanupCtx).WithValues("namespace", namespace, "name", alias, "createdCheckpointID", createdCheckpointID)
	checkpoints, _, err := sc.manager.ListCheckpoints(cleanupCtx, infra.SelectSucceededCheckpointsOptions{
		Namespace: namespace,
		User:      owner,
		Alias:     alias,
	}, nil)
	if err != nil {
		log.Error(err, "failed to list named snapshots; leaving duplicates to the next replacement")
		return createdCheckpointID
	}
	if len(checkpoints) == 0 {
		// Creation waited for the Succeeded event, so an empty list means the
		// checkpoint was removed before reconciliation and no holder survives.
		log.Info("new checkpoint not visible in the alias list; skipping reconciliation")
		return createdCheckpointID
	}
	newest := slices.MaxFunc(checkpoints, compareCheckpoints)
	for _, cp := range checkpoints {
		if cp.CheckpointID == newest.CheckpointID {
			continue
		}
		if err := sc.deleteCheckpointForAlias(cleanupCtx, namespace, owner, cp.CheckpointID); err != nil {
			log.Error(err, "failed to delete superseded named snapshot", "staleCheckpointID", cp.CheckpointID)
		}
	}
	if newest.CheckpointID != createdCheckpointID {
		log.Info("named snapshot superseded by a newer checkpoint", "keptCheckpointID", newest.CheckpointID)
	}
	return newest.CheckpointID
}

func (sc *Controller) deleteCheckpointForAlias(ctx context.Context, namespace, owner, checkpointID string) error {
	err := sc.manager.DeleteCheckpoint(ctx, owner, infra.DeleteCheckpointOptions{Namespace: namespace, CheckpointID: checkpointID})
	if managererrors.GetErrCode(err) == managererrors.ErrorNotFound {
		return nil
	}
	return err
}

// compareCheckpoints orders checkpoints by creation time, then by name.
func compareCheckpoints(a, b infra.CheckpointInfo) int {
	return cmp.Or(strings.Compare(a.CreationTimestamp, b.CreationTimestamp), strings.Compare(a.Name, b.Name))
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
