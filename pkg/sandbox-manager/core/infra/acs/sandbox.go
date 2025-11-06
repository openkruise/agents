package acs

import (
	"context"
	"fmt"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/k8s"
)

type Sandbox struct {
	*k8s.Sandbox
}

func (s *Sandbox) Pause(ctx context.Context) error {
	return s.SetPause(ctx, true)
}

func (s *Sandbox) Resume(ctx context.Context) error {
	return s.SetPause(ctx, false)
}

func (s *Sandbox) SetPause(ctx context.Context, pause bool) error {
	pod := s.Pod
	state := pod.Labels[consts.LabelSandboxState]
	var expectedState, expectedAnnotation string
	if pause {
		expectedState = consts.SandboxStatePaused
		expectedAnnotation = "true"
	} else {
		expectedState = consts.SandboxStateRunning
		expectedAnnotation = "false"
	}
	if state == expectedState {
		return nil // no need to patch
	}
	return s.Patch(ctx, fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"},"annotations":{"%s":"%s"}}}`,
		consts.LabelSandboxState, expectedState, consts.AnnotationACSPause, expectedAnnotation))
}
