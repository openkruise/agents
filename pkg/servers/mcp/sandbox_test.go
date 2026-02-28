/*
Copyright 2026 The Kruise Authors.

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

package mcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSandbox implements infra.Sandbox interface for testing
type mockSandbox struct {
	metav1.ObjectMeta
	sandboxID   string
	state       string
	stateReason string
	route       proxy.Route
	template    string
	image       string
	timeout     infra.TimeoutOptions
	claimTime   time.Time
	killErr     error
	annotations map[string]string
}

func newMockSandbox(sandboxID string) *mockSandbox {
	return &mockSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        sandboxID,
			Namespace:   "default",
			Annotations: make(map[string]string),
		},
		sandboxID:   sandboxID,
		state:       v1alpha1.SandboxStateRunning,
		stateReason: "",
		route: proxy.Route{
			ID:    sandboxID,
			IP:    "10.0.0.1",
			Owner: "test-user",
		},
		template:    "test-template",
		claimTime:   time.Now(),
		annotations: make(map[string]string),
	}
}

func (m *mockSandbox) Pause(ctx context.Context, opts infra.PauseOptions) error { return nil }
func (m *mockSandbox) Resume(ctx context.Context) error                         { return nil }
func (m *mockSandbox) GetSandboxID() string                                     { return m.sandboxID }
func (m *mockSandbox) GetRoute() proxy.Route                                    { return m.route }
func (m *mockSandbox) GetState() (string, string)                               { return m.state, m.stateReason }
func (m *mockSandbox) GetTemplate() string                                      { return m.template }
func (m *mockSandbox) GetResource() infra.SandboxResource                       { return infra.SandboxResource{} }
func (m *mockSandbox) SetImage(image string)                                    { m.image = image }
func (m *mockSandbox) GetImage() string                                         { return m.image }
func (m *mockSandbox) SetTimeout(opts infra.TimeoutOptions)                     { m.timeout = opts }
func (m *mockSandbox) SaveTimeout(ctx context.Context, opts infra.TimeoutOptions) error {
	m.timeout = opts
	return nil
}
func (m *mockSandbox) GetTimeout() infra.TimeoutOptions                        { return m.timeout }
func (m *mockSandbox) GetClaimTime() (time.Time, error)                        { return m.claimTime, nil }
func (m *mockSandbox) Kill(ctx context.Context) error                          { return m.killErr }
func (m *mockSandbox) InplaceRefresh(ctx context.Context, deepcopy bool) error { return nil }
func (m *mockSandbox) Request(ctx context.Context, method, path string, port int, body io.Reader) (*http.Response, error) {
	return nil, nil
}
func (m *mockSandbox) CSIMount(ctx context.Context, driver string, request string) error { return nil }
func (m *mockSandbox) GetRuntimeURL() string                                             { return "http://10.0.0.1:49982" }
func (m *mockSandbox) GetAccessToken() string                                            { return "mock-token" }
func (m *mockSandbox) GetAnnotations() map[string]string                                 { return m.annotations }
func (m *mockSandbox) SetAnnotations(annotations map[string]string)                      { m.annotations = annotations }

// mockSandboxOperator implements SandboxOperator interface for testing
type mockSandboxOperator struct {
	claimSandboxFunc      func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error)
	getClaimedSandboxFunc func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error)
}

func (m *mockSandboxOperator) ClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
	if m.claimSandboxFunc != nil {
		return m.claimSandboxFunc(ctx, opts)
	}
	return nil, errors.New("not implemented")
}

func (m *mockSandboxOperator) GetClaimedSandbox(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
	if m.getClaimedSandboxFunc != nil {
		return m.getClaimedSandboxFunc(ctx, userID, sandboxID)
	}
	return nil, errors.New("not implemented")
}

func TestCreateSandboxWithOperator(t *testing.T) {
	t.Run("creates sandbox successfully", func(t *testing.T) {
		mockSbx := newMockSandbox("sandbox-123")
		mockSbx.state = v1alpha1.SandboxStateRunning

		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				assert.Equal(t, "user-1", opts.User)
				assert.Equal(t, "template-1", opts.Template)
				// Call modifier to test annotation setting
				if opts.Modifier != nil {
					opts.Modifier(mockSbx)
				}
				return mockSbx, nil
			},
		}

		ctx := context.Background()
		info, err := CreateSandboxWithOperator(ctx, operator, "user-1", "session-1", "template-1", 5*time.Minute)

		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Equal(t, "sandbox-123", info.SandboxID)
		assert.Equal(t, v1alpha1.SandboxStateRunning, info.State)
		assert.NotEmpty(t, info.AccessToken)

		// Verify annotations were set
		assert.Equal(t, "session-1", mockSbx.annotations[v1alpha1.AnnotationMCPSessionID])
	})

	t.Run("handles claim error", func(t *testing.T) {
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				return nil, errors.New("no available sandbox")
			},
		}

		ctx := context.Background()
		info, err := CreateSandboxWithOperator(ctx, operator, "user-1", "session-1", "template-1", 5*time.Minute)

		assert.Error(t, err)
		assert.Nil(t, info)
		assert.Contains(t, err.Error(), "failed to claim sandbox")
	})

	t.Run("sets timeout when TTL > 0", func(t *testing.T) {
		mockSbx := newMockSandbox("sandbox-ttl")
		var capturedTimeout infra.TimeoutOptions

		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				if opts.Modifier != nil {
					opts.Modifier(mockSbx)
					capturedTimeout = mockSbx.timeout
				}
				return mockSbx, nil
			},
		}

		ctx := context.Background()
		_, err := CreateSandboxWithOperator(ctx, operator, "user-1", "session-1", "template-1", 10*time.Minute)

		require.NoError(t, err)
		assert.False(t, capturedTimeout.ShutdownTime.IsZero())
	})

	t.Run("does not set timeout when TTL is 0", func(t *testing.T) {
		mockSbx := newMockSandbox("sandbox-no-ttl")

		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				if opts.Modifier != nil {
					opts.Modifier(mockSbx)
				}
				return mockSbx, nil
			},
		}

		ctx := context.Background()
		_, err := CreateSandboxWithOperator(ctx, operator, "user-1", "session-1", "template-1", 0)

		require.NoError(t, err)
		assert.True(t, mockSbx.timeout.ShutdownTime.IsZero())
	})
}

func TestGetSandboxWithOperator(t *testing.T) {
	t.Run("gets sandbox successfully", func(t *testing.T) {
		mockSbx := newMockSandbox("sandbox-get")
		mockSbx.state = v1alpha1.SandboxStateRunning
		mockSbx.annotations[v1alpha1.AnnotationRuntimeAccessToken] = "access-token-123"

		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				assert.Equal(t, "user-1", userID)
				assert.Equal(t, "sandbox-get", sandboxID)
				return mockSbx, nil
			},
		}

		ctx := context.Background()
		info, err := GetSandboxWithOperator(ctx, operator, "user-1", "sandbox-get")

		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Equal(t, "sandbox-get", info.SandboxID)
		assert.Equal(t, v1alpha1.SandboxStateRunning, info.State)
		assert.Equal(t, "access-token-123", info.AccessToken)
	})

	t.Run("returns error when sandbox not found", func(t *testing.T) {
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return nil, errors.New("sandbox not found")
			},
		}

		ctx := context.Background()
		info, err := GetSandboxWithOperator(ctx, operator, "user-1", "non-existent")

		assert.Error(t, err)
		assert.Nil(t, info)
		assert.Contains(t, err.Error(), "sandbox not found")
	})
}

func TestDeleteSandboxWithOperator(t *testing.T) {
	t.Run("deletes sandbox successfully", func(t *testing.T) {
		mockSbx := newMockSandbox("sandbox-delete")
		mockSbx.killErr = nil

		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		ctx := context.Background()
		err := DeleteSandboxWithOperator(ctx, operator, "user-1", "sandbox-delete")

		assert.NoError(t, err)
	})

	t.Run("returns error when kill fails", func(t *testing.T) {
		mockSbx := newMockSandbox("sandbox-kill-fail")
		mockSbx.killErr = errors.New("kill operation failed")

		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		ctx := context.Background()
		err := DeleteSandboxWithOperator(ctx, operator, "user-1", "sandbox-kill-fail")

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to delete sandbox")
	})
}

func TestSandboxOperatorInterface(t *testing.T) {
	t.Run("mock operator implements interface", func(t *testing.T) {
		var operator SandboxOperator = &mockSandboxOperator{}
		assert.NotNil(t, operator)
	})
}
