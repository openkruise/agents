package sandboxset

import (
	"github.com/openkruise/agents/pkg/webhook/sandboxtemplate/mutating"
	"github.com/openkruise/agents/pkg/webhook/sandboxtemplate/validating"
	"github.com/openkruise/agents/pkg/webhook/types"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func GetHandlerGetters() []types.HandlerGetter {
	return []types.HandlerGetter{
		func(mgr manager.Manager) types.Handler {
			return &mutating.Defaulter{
				Client:  mgr.GetClient(),
				Decoder: admission.NewDecoder(mgr.GetScheme()),
			}
		},
		func(mgr manager.Manager) types.Handler {
			return &validating.ValidatingHandler{
				Client:  mgr.GetClient(),
				Decoder: admission.NewDecoder(mgr.GetScheme()),
			}
		},
	}
}
