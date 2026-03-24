package e2b

import (
	"net/http"

	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/klog/v2"
)

// DeleteTemplate deletes a template (checkpoint and its associated sandbox template)
func (sc *Controller) DeleteTemplate(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	templateID := r.PathValue("templateID")
	ctx := r.Context()
	log := klog.FromContext(ctx)
	log.Info("delete template request received", "templateID", templateID)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User not found",
		}
	}

	if err := sc.manager.DeleteCheckpoint(ctx, user.ID.String(), templateID); err != nil {
		log.Error(err, "failed to delete template", "templateID", templateID)
		switch errors.GetErrCode(err) {
		case errors.ErrorNotFound:
			fallthrough
		case errors.ErrorNotAllowed:
			// Return 204 No Content as success for not found or not allowed errors
			return web.ApiResponse[struct{}]{
				Code: http.StatusNoContent,
			}, nil
		default:
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: "Failed to delete template",
			}
		}
	}

	log.Info("template deleted", "templateID", templateID)
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
