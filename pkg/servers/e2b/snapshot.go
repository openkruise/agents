package e2b

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
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
	sbx, apiErr := sc.getSandboxOfUser(ctx, sandboxID)
	if apiErr != nil {
		return web.ApiResponse[*models.Snapshot]{}, apiErr
	}
	if state, reason := sbx.GetState(); state != v1alpha1.SandboxStateRunning {
		log.Info("cannot create snapshot: sandbox is not running", "state", state, "reason", reason)
		return web.ApiResponse[*models.Snapshot]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Sandbox %s is not running", sandboxID),
		}
	}
	checkpointID, err := sbx.CreateCheckpoint(ctx, infra.CreateCheckpointOptions{
		KeepRunning:        request.Extensions.KeepRunning,
		TTL:                request.Extensions.TTL,
		PersistentContents: request.Extensions.PersistentContents,
		WaitSuccessTimeout: time.Duration(request.Extensions.WaitSuccessSeconds) * time.Second,
	})
	if err != nil {
		log.Error(err, "failed to create checkpoint")
		SnapshotTotal.WithLabelValues("failure").Inc()
		return web.ApiResponse[*models.Snapshot]{}, &web.ApiError{
			Message: err.Error(),
		}
	}
	SnapshotDuration.Observe(time.Since(start).Seconds())
	SnapshotTotal.WithLabelValues("success").Inc()
	return web.ApiResponse[*models.Snapshot]{
		Code: http.StatusCreated,
		Body: &models.Snapshot{
			SnapshotID: checkpointID,
		},
	}, nil
}

func (sc *Controller) parseCreateSnapshotRequest(r *http.Request) (models.NewSnapshotRequest, *web.ApiError) {
	var request models.NewSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, &web.ApiError{
			Message: err.Error(),
		}
	}

	if err := request.ParseExtensions(r.Header); err != nil {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Bad extension param: %s", err.Error()),
		}
	}
	return request, nil
}
