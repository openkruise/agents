package utils

import (
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/utils/ptr"
)

func GetSandboxAddress(sandboxId, domain string, port int32) string {
	return fmt.Sprintf("%d-%s.%s", port, sandboxId, domain)
}

func SandboxIngressRule(sandboxId, domain string, port int32) networkingv1.IngressRule {
	return networkingv1.IngressRule{
		// This rule is for chrome websocket only
		Host: GetSandboxAddress(sandboxId, domain, port),
		IngressRuleValue: networkingv1.IngressRuleValue{
			HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{
					{
						Path:     "/",
						PathType: ptr.To(networkingv1.PathTypePrefix),
						Backend: networkingv1.IngressBackend{
							Service: &networkingv1.IngressServiceBackend{
								Name: sandboxId,
								Port: networkingv1.ServiceBackendPort{
									Number: port,
								},
							},
						},
					},
				},
			},
		},
	}
}
