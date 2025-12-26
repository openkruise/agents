package e2b

import (
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

var (
	BlackListPrefix = []string{models.InternalPrefix, agentsv1alpha1.InternalPrefix}
)
