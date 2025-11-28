package e2b

import agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"

const (
	InternalPrefix           = "e2b." + agentsv1alpha1.InternalPrefix
	AnnotationShouldInitEnvd = InternalPrefix + "should-init-envd"
)
