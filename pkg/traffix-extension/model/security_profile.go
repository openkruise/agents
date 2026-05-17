package model

import (
	"github.com/openkruise/agents/api/v1alpha1"
	"k8s.io/apimachinery/pkg/labels"
)

type SecurityProfile struct {
	Profile  *v1alpha1.SecurityProfile
	Selector labels.Selector
}
