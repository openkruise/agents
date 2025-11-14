package types

import (
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type Handler interface {
	admission.Handler
	Path() string
	Enabled() bool
}

type HandlerGetter = func(manager.Manager) Handler

type Result int

const (
	Skip Result = iota
	Patch
	Deny
)

type Response struct {
	Result  Result
	Message string
}
