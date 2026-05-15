/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sandbox_manager

import (
	"context"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

type ConnectOrWakeInput struct {
	PreState       string
	AutoPause      bool
	PreEndAt       time.Time
	Baseline       timeout.Options
	NewEndAt       time.Time
	SetAnnotations map[string]string
}

func (m *SandboxManager) ConnectOrWake(ctx context.Context, sbx infra.Sandbox, in ConnectOrWakeInput) error {
	if in.PreState == v1alpha1.SandboxStatePaused {
		if err := m.ResumeSandbox(ctx, sbx, infra.ResumeOptions{}); err != nil {
			return err
		}
	}

	if in.PreEndAt.IsZero() && in.NewEndAt.IsZero() && len(in.SetAnnotations) == 0 {
		return nil
	}

	opts := timeout.Options{
		Baseline:       &in.Baseline,
		SetAnnotations: in.SetAnnotations,
	}
	if !in.NewEndAt.IsZero() {
		if in.AutoPause {
			opts.PauseTime = in.NewEndAt
			opts.ShutdownTime = time.Now().Add(m.maxTimeout)
		} else {
			opts.ShutdownTime = in.NewEndAt
		}
	}

	policy := timeout.UpdatePolicyExtendOnly
	if in.PreState == v1alpha1.SandboxStatePaused {
		policy = timeout.UpdatePolicyBaselineAware
	}
	_, err := sbx.SaveTimeoutWithPolicy(ctx, opts, policy)
	return err
}
