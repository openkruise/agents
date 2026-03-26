package pod

import (
	"github.com/openkruise/agents/pkg/webhook/pod/validating"
	"github.com/openkruise/agents/pkg/webhook/types"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func GetHandlerGetters() []types.HandlerGetter {
	return []types.HandlerGetter{
		func(mgr manager.Manager) types.Handler {
			return &validating.PodValidatingHandler{
				Client:  mgr.GetClient(),
				Decoder: admission.NewDecoder(mgr.GetScheme()),
			}
		},
	}
}
