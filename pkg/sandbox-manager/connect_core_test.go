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
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

func TestSandboxManagerBuilder_WithMaxTimeout(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
	}{
		{
			name: "sets max timeout",
			d:    10 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := NewSandboxManagerBuilder(testManagerOptions())
			got := builder.WithMaxTimeout(tt.d)

			assert.Same(t, builder, got)
			assert.Equal(t, tt.d, builder.instance.maxTimeout)
		})
	}
}

func TestSandboxManager_ConnectOrWake(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	baseline := timeout.Options{ShutdownTime: now.Add(10 * time.Minute)}
	annotation := map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:300s"}
	resumeErr := errors.New("resume failed")

	tests := []struct {
		name                 string
		input                ConnectOrWakeInput
		resumeErr            error
		expectResumeCalls    int
		expectSaveCalls      int
		expectPolicy         timeout.UpdatePolicy
		expectPauseTime      time.Time
		expectShutdownTime   time.Time
		expectBaseline       timeout.Options
		expectSetAnnotations map[string]string
		expectError          string
	}{
		{
			name: "paused resumes then writes baseline aware timeout",
			input: ConnectOrWakeInput{
				PreState: v1alpha1.SandboxStatePaused,
				PreEndAt: baseline.ShutdownTime,
				Baseline: baseline,
				NewEndAt: now.Add(5 * time.Minute),
			},
			expectResumeCalls:  1,
			expectSaveCalls:    1,
			expectPolicy:       timeout.UpdatePolicyBaselineAware,
			expectShutdownTime: now.Add(5 * time.Minute),
			expectBaseline:     baseline,
		},
		{
			name: "running writes extend only timeout",
			input: ConnectOrWakeInput{
				PreState: v1alpha1.SandboxStateRunning,
				PreEndAt: baseline.ShutdownTime,
				Baseline: baseline,
				NewEndAt: now.Add(15 * time.Minute),
			},
			expectSaveCalls:    1,
			expectPolicy:       timeout.UpdatePolicyExtendOnly,
			expectShutdownTime: now.Add(15 * time.Minute),
			expectBaseline:     baseline,
		},
		{
			name: "never timeout with no annotation fast returns",
			input: ConnectOrWakeInput{
				PreState: v1alpha1.SandboxStateRunning,
				Baseline: timeout.Options{},
			},
		},
		{
			name: "never timeout with annotation writes",
			input: ConnectOrWakeInput{
				PreState:       v1alpha1.SandboxStateRunning,
				Baseline:       timeout.Options{},
				SetAnnotations: annotation,
			},
			expectSaveCalls:      1,
			expectPolicy:         timeout.UpdatePolicyBaselineAware,
			expectSetAnnotations: annotation,
		},
		{
			name: "auto pause finite timeout sets pause and max shutdown",
			input: ConnectOrWakeInput{
				PreState:  v1alpha1.SandboxStateRunning,
				AutoPause: true,
				PreEndAt:  baseline.ShutdownTime,
				Baseline:  baseline,
				NewEndAt:  now.Add(5 * time.Minute),
			},
			expectSaveCalls:    1,
			expectPolicy:       timeout.UpdatePolicyExtendOnly,
			expectPauseTime:    now.Add(5 * time.Minute),
			expectShutdownTime: now.Add(30 * time.Minute),
			expectBaseline:     baseline,
		},
		{
			name: "manual finite timeout sets only shutdown",
			input: ConnectOrWakeInput{
				PreState: v1alpha1.SandboxStateRunning,
				PreEndAt: baseline.ShutdownTime,
				Baseline: baseline,
				NewEndAt: now.Add(5 * time.Minute),
			},
			expectSaveCalls:    1,
			expectPolicy:       timeout.UpdatePolicyExtendOnly,
			expectShutdownTime: now.Add(5 * time.Minute),
			expectBaseline:     baseline,
		},
		{
			name: "zero new end with annotation clears timeout fields",
			input: ConnectOrWakeInput{
				PreState:       v1alpha1.SandboxStateRunning,
				PreEndAt:       baseline.ShutdownTime,
				Baseline:       baseline,
				SetAnnotations: annotation,
			},
			expectSaveCalls:      1,
			expectPolicy:         timeout.UpdatePolicyBaselineAware,
			expectBaseline:       baseline,
			expectSetAnnotations: annotation,
		},
		{
			name: "resume error propagates and skips save",
			input: ConnectOrWakeInput{
				PreState: v1alpha1.SandboxStatePaused,
				PreEndAt: baseline.ShutdownTime,
				Baseline: baseline,
				NewEndAt: now.Add(5 * time.Minute),
			},
			resumeErr:         resumeErr,
			expectResumeCalls: 1,
			expectError:       "resume failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := newFakeSandbox("sandbox")
			sbx.resumeErr = tt.resumeErr
			manager := &SandboxManager{
				proxy:      proxy.NewServer(testManagerOptions()),
				maxTimeout: 30 * time.Minute,
			}

			start := time.Now()
			err := manager.ConnectOrWake(t.Context(), sbx, tt.input)
			end := time.Now()

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectResumeCalls, sbx.resumeCalls)
			assert.Equal(t, tt.expectSaveCalls, sbx.saveCalls)
			if tt.expectSaveCalls == 0 {
				return
			}
			assert.Equal(t, tt.expectPolicy, sbx.lastPolicy)
			require.NotNil(t, sbx.lastOptions.Baseline)
			assert.Equal(t, tt.expectBaseline, *sbx.lastOptions.Baseline)
			assert.Equal(t, tt.expectSetAnnotations, sbx.lastOptions.SetAnnotations)
			if !tt.expectPauseTime.IsZero() {
				assert.WithinDuration(t, tt.expectPauseTime, sbx.lastOptions.PauseTime, 2*time.Second)
			} else {
				assert.True(t, sbx.lastOptions.PauseTime.IsZero())
			}
			if tt.input.AutoPause && !tt.input.NewEndAt.IsZero() {
				assert.True(t, !sbx.lastOptions.ShutdownTime.Before(start.Add(manager.maxTimeout)))
				assert.True(t, !sbx.lastOptions.ShutdownTime.After(end.Add(manager.maxTimeout+2*time.Second)))
			} else if !tt.expectShutdownTime.IsZero() {
				assert.WithinDuration(t, tt.expectShutdownTime, sbx.lastOptions.ShutdownTime, 2*time.Second)
			} else {
				assert.True(t, sbx.lastOptions.ShutdownTime.IsZero())
			}
		})
	}
}

func TestSandboxManager_ConnectOrWakeZeroNewEndPolicySemantics(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	annotation := map[string]string{v1alpha1.AnnotationWakeOnTraffic: "timeout:300s"}

	tests := []struct {
		name            string
		initialTimeout  timeout.Options
		input           ConnectOrWakeInput
		expectUpdated   bool
		expectFinalZero bool
	}{
		{
			name:           "finite running annotation-only write clears timeout with real policy semantics",
			initialTimeout: timeout.Options{ShutdownTime: now.Add(10 * time.Minute)},
			input: ConnectOrWakeInput{
				PreState:       v1alpha1.SandboxStateRunning,
				PreEndAt:       now.Add(10 * time.Minute),
				Baseline:       timeout.Options{ShutdownTime: now.Add(10 * time.Minute)},
				SetAnnotations: annotation,
			},
			expectUpdated:   true,
			expectFinalZero: true,
		},
		{
			name:           "never-timeout annotation-only write calls save but policy skips update",
			initialTimeout: timeout.Options{},
			input: ConnectOrWakeInput{
				PreState:       v1alpha1.SandboxStateRunning,
				Baseline:       timeout.Options{},
				SetAnnotations: annotation,
			},
			expectUpdated:   false,
			expectFinalZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newPolicyTimeoutState(tt.initialTimeout)
			sbx := newFakeSandbox("sandbox")
			sbx.timeout = tt.initialTimeout
			sbx.saveFn = state.save
			manager := &SandboxManager{
				proxy:      proxy.NewServer(testManagerOptions()),
				maxTimeout: 30 * time.Minute,
			}

			err := manager.ConnectOrWake(t.Context(), sbx, tt.input)
			require.NoError(t, err)

			assert.Equal(t, 1, sbx.saveCalls)
			results := state.results()
			require.Len(t, results, 1)
			assert.Equal(t, tt.expectUpdated, results[0].Updated)
			finalTimeout := state.get()
			if tt.expectFinalZero {
				assert.True(t, finalTimeout.ShutdownTime.IsZero())
				assert.True(t, finalTimeout.PauseTime.IsZero())
			}
		})
	}
}

func testManagerOptions() config.SandboxManagerOptions {
	return config.SandboxManagerOptions{}
}

type fakeSandbox struct {
	mu sync.Mutex

	metav1.ObjectMeta

	id       string
	state    string
	reason   string
	raw      *v1alpha1.Sandbox
	route    proxy.Route
	timeout  timeout.Options
	resource infra.SandboxResource

	resumeErr  error
	saveErr    error
	refreshErr error
	getClaimAt time.Time

	resumeFn     func(context.Context) error
	saveFn       func(context.Context, timeout.Options, timeout.UpdatePolicy) (infra.TimeoutUpdateResult, error)
	getTimeoutFn func() timeout.Options

	resumeCalls  int
	saveCalls    int
	refreshCalls int
	lastOptions  timeout.Options
	lastPolicy   timeout.UpdatePolicy
}

func newFakeSandbox(id string) *fakeSandbox {
	raw := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:            id,
			Namespace:       "default",
			Annotations:     map[string]string{},
			ResourceVersion: "1",
		},
	}
	return &fakeSandbox{
		ObjectMeta: raw.ObjectMeta,
		id:         id,
		state:      v1alpha1.SandboxStateRunning,
		raw:        raw,
		route: proxy.Route{
			ID:              id,
			State:           v1alpha1.SandboxStateRunning,
			ResourceVersion: "1",
		},
		getClaimAt: time.Now(),
	}
}

func (f *fakeSandbox) Pause(context.Context, infra.PauseOptions) error {
	return nil
}

func (f *fakeSandbox) Resume(ctx context.Context, _ infra.ResumeOptions) error {
	f.mu.Lock()
	f.resumeCalls++
	resumeFn := f.resumeFn
	resumeErr := f.resumeErr
	f.mu.Unlock()
	if resumeFn != nil {
		return resumeFn(ctx)
	}
	return resumeErr
}

func (f *fakeSandbox) GetSandboxID() string {
	return f.id
}

func (f *fakeSandbox) GetRoute() proxy.Route {
	return f.route
}

func (f *fakeSandbox) GetState() (string, string) {
	return f.state, f.reason
}

func (f *fakeSandbox) GetTemplate() string {
	return "template"
}

func (f *fakeSandbox) GetResource() infra.SandboxResource {
	return f.resource
}

func (f *fakeSandbox) SetImage(string) {
}

func (f *fakeSandbox) GetImage() string {
	return "image"
}

func (f *fakeSandbox) SetPodLabels(map[string]string) {
}

func (f *fakeSandbox) GetPodLabels() map[string]string {
	return nil
}

func (f *fakeSandbox) SetTimeout(opts timeout.Options) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timeout = opts
}

func (f *fakeSandbox) SaveTimeoutWithPolicy(ctx context.Context, opts timeout.Options, policy timeout.UpdatePolicy) (infra.TimeoutUpdateResult, error) {
	f.mu.Lock()
	f.saveCalls++
	f.lastOptions = opts
	f.lastPolicy = policy
	saveFn := f.saveFn
	saveErr := f.saveErr
	f.mu.Unlock()
	if saveFn != nil {
		return saveFn(ctx, opts, policy)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timeout = opts
	if saveErr != nil {
		return infra.TimeoutUpdateResult{}, saveErr
	}
	return infra.TimeoutUpdateResult{Updated: true}, nil
}

func (f *fakeSandbox) GetTimeout() timeout.Options {
	if f.getTimeoutFn != nil {
		return f.getTimeoutFn()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.timeout
}

func (f *fakeSandbox) GetClaimTime() (time.Time, error) {
	return f.getClaimAt, nil
}

func (f *fakeSandbox) Kill(context.Context) error {
	return nil
}

func (f *fakeSandbox) InplaceRefresh(context.Context, bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshCalls++
	return f.refreshErr
}

func (f *fakeSandbox) Request(context.Context, string, string, int, io.Reader) (*http.Response, error) {
	return nil, nil
}

func (f *fakeSandbox) CSIMount(context.Context, string, string) error {
	return nil
}

func (f *fakeSandbox) CreateCheckpoint(context.Context, infra.CreateCheckpointOptions) (string, error) {
	return "", nil
}

func (f *fakeSandbox) GetSandboxCR() *v1alpha1.Sandbox {
	return f.raw
}

type fakeInfrastructure struct {
	sandbox infra.Sandbox
	err     error
}

func (f *fakeInfrastructure) Run(context.Context) error {
	return nil
}

func (f *fakeInfrastructure) Stop(context.Context) {
}

func (f *fakeInfrastructure) HasTemplate(context.Context, infra.HasTemplateOptions) bool {
	return true
}

func (f *fakeInfrastructure) HasCheckpoint(context.Context, infra.HasCheckpointOptions) bool {
	return true
}

func (f *fakeInfrastructure) GetCache() cache.Provider {
	return nil
}

func (f *fakeInfrastructure) LoadDebugInfo() map[string]any {
	return nil
}

func (f *fakeInfrastructure) SelectSandboxes(context.Context, infra.SelectSandboxesOptions) ([]infra.Sandbox, error) {
	return nil, nil
}

func (f *fakeInfrastructure) GetClaimedSandbox(context.Context, infra.GetClaimedSandboxOptions) (infra.Sandbox, error) {
	return f.sandbox, f.err
}

func (f *fakeInfrastructure) SelectSucceededCheckpoints(context.Context, infra.SelectSucceededCheckpointsOptions) ([]infra.CheckpointInfo, error) {
	return nil, nil
}

func (f *fakeInfrastructure) ClaimSandbox(context.Context, infra.ClaimSandboxOptions) (infra.Sandbox, infra.ClaimMetrics, error) {
	return nil, infra.ClaimMetrics{}, nil
}

func (f *fakeInfrastructure) CloneSandbox(context.Context, infra.CloneSandboxOptions) (infra.Sandbox, infra.CloneMetrics, error) {
	return nil, infra.CloneMetrics{}, nil
}

func (f *fakeInfrastructure) DeleteCheckpoint(context.Context, infra.DeleteCheckpointOptions) error {
	return nil
}
