package e2b

import (
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var (
	BlackListPrefix = []string{agentsv1alpha1.E2BPrefix, agentsv1alpha1.InternalPrefix}
)
