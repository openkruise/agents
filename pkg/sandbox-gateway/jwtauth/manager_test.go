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

package jwtauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/identity/oidc"
)

const testTimeout = 2 * time.Second

type fakeVerifier struct {
	name string
}

func (v *fakeVerifier) Verify(string) (*oidc.TrafficAccessTokenClaims, error) {
	return nil, nil
}

type fakeReader struct {
	name string
}

func (r *fakeReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return nil
}

func (r *fakeReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return nil
}

func testOptions(name string) oidc.Options {
	return oidc.Options{DiscoveryURL: "https://" + name + ".example.test/.well-known/openid-configuration"}
}

func newTestManager(options oidc.Options, loader VerifierLoader) *Manager {
	return NewManagerWithDependencies(func() (oidc.Options, error) {
		return options, nil
	}, loader, time.Millisecond, 4*time.Millisecond)
}

func startManager(t *testing.T, manager *Manager) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- manager.Start(ctx)
	}()
	require.Eventually(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.started
	}, testTimeout, time.Millisecond)
	return cancel, done
}

func stopManager(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(testTimeout):
		t.Fatal("manager did not stop after context cancellation")
	}
}

func waitForState(t *testing.T, manager *Manager, expected State) {
	t.Helper()
	require.Eventually(t, func() bool {
		return manager.State() == expected
	}, testTimeout, time.Millisecond)
}

func TestManagerReadiness(t *testing.T) {
	tests := []struct {
		name        string
		configure   bool
		enabled     bool
		expectState State
		expectError string
	}{
		{
			name:        "awaiting configuration",
			expectState: AwaitingConfig,
			expectError: "awaiting configuration",
		},
		{
			name:        "disabled is ready",
			configure:   true,
			enabled:     false,
			expectState: Disabled,
		},
		{
			name:        "enabled is initializing",
			configure:   true,
			enabled:     true,
			expectState: Initializing,
			expectError: "initializing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newTestManager(testOptions("readiness"), nil)
			if tt.configure {
				require.NoError(t, manager.Configure(tt.enabled))
			}

			assert.Equal(t, tt.expectState, manager.State())
			assert.Nil(t, manager.Current())
			assert.False(t, manager.NeedLeaderElection())
			err := manager.Ready()
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestManagerConfigure(t *testing.T) {
	optionsA := testOptions("a")
	optionsB := testOptions("b")
	tests := []struct {
		name        string
		run         func(*Manager, *oidc.Options) error
		expectState State
		expectError string
	}{
		{
			name: "disabled configuration is idempotent",
			run: func(manager *Manager, _ *oidc.Options) error {
				require.NoError(t, manager.Configure(false))
				return manager.Configure(false)
			},
			expectState: Disabled,
		},
		{
			name: "enabled configuration is idempotent",
			run: func(manager *Manager, _ *oidc.Options) error {
				require.NoError(t, manager.Configure(true))
				return manager.Configure(true)
			},
			expectState: Initializing,
		},
		{
			name: "disabled then enabled conflicts",
			run: func(manager *Manager, _ *oidc.Options) error {
				require.NoError(t, manager.Configure(false))
				return manager.Configure(true)
			},
			expectState: Disabled,
			expectError: "already configured with enabled=false",
		},
		{
			name: "enabled then disabled conflicts",
			run: func(manager *Manager, _ *oidc.Options) error {
				require.NoError(t, manager.Configure(true))
				return manager.Configure(false)
			},
			expectState: Initializing,
			expectError: "already configured with enabled=true",
		},
		{
			name: "changed options conflict",
			run: func(manager *Manager, current *oidc.Options) error {
				require.NoError(t, manager.Configure(true))
				*current = optionsB
				return manager.Configure(true)
			},
			expectState: Initializing,
			expectError: "different options",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := optionsA
			manager := NewManagerWithDependencies(func() (oidc.Options, error) {
				return current, nil
			}, nil, time.Millisecond, time.Millisecond)

			err := tt.run(manager, &current)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
			assert.Equal(t, tt.expectState, manager.State())
		})
	}
}

func TestManagerConfigureOptionsError(t *testing.T) {
	tests := []struct {
		name        string
		failures    int
		configure   func(*Manager) error
		expectState State
		expectError string
		expectCalls int
	}{
		{
			name:     "first failure leaves manager unconfigured",
			failures: 1,
			configure: func(manager *Manager) error {
				return manager.Configure(true)
			},
			expectState: AwaitingConfig,
			expectError: "invalid local options",
			expectCalls: 1,
		},
		{
			name:     "configuration can retry after source failure",
			failures: 1,
			configure: func(manager *Manager) error {
				require.Error(t, manager.Configure(true))
				return manager.Configure(true)
			},
			expectState: Initializing,
			expectCalls: 2,
		},
		{
			name: "repeated configuration reports source failure",
			configure: func(manager *Manager) error {
				require.NoError(t, manager.Configure(true))
				return manager.Configure(true)
			},
			expectState: Initializing,
			expectError: "invalid local options",
			expectCalls: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			manager := NewManagerWithDependencies(func() (oidc.Options, error) {
				calls++
				if calls <= tt.failures || (tt.name == "repeated configuration reports source failure" && calls == 2) {
					return oidc.Options{}, errors.New("invalid local options")
				}
				return testOptions("valid"), nil
			}, nil, time.Millisecond, time.Millisecond)

			err := tt.configure(manager)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
			assert.Equal(t, tt.expectState, manager.State())
			assert.Equal(t, tt.expectCalls, calls)
		})
	}
}

func TestManagerSetReader(t *testing.T) {
	readerA := &fakeReader{name: "a"}
	readerB := &fakeReader{name: "b"}
	var typedNil *fakeReader
	tests := []struct {
		name        string
		run         func(*Manager) error
		expectError string
	}{
		{
			name: "nil reader is rejected",
			run: func(manager *Manager) error {
				return manager.SetReader(nil)
			},
			expectError: "must not be nil",
		},
		{
			name: "typed nil reader is rejected",
			run: func(manager *Manager) error {
				return manager.SetReader(typedNil)
			},
			expectError: "must not be nil",
		},
		{
			name: "first reader is accepted",
			run: func(manager *Manager) error {
				return manager.SetReader(readerA)
			},
		},
		{
			name: "same reader is idempotent",
			run: func(manager *Manager) error {
				require.NoError(t, manager.SetReader(readerA))
				return manager.SetReader(readerA)
			},
		},
		{
			name: "different reader conflicts",
			run: func(manager *Manager) error {
				require.NoError(t, manager.SetReader(readerA))
				return manager.SetReader(readerB)
			},
			expectError: "already set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newTestManager(testOptions("reader"), nil)
			err := tt.run(manager)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestManagerStartupOrder(t *testing.T) {
	tests := []struct {
		name  string
		steps []string
	}{
		{
			name:  "configure reader start",
			steps: []string{"configure", "reader", "start"},
		},
		{
			name:  "reader start configure",
			steps: []string{"reader", "start", "configure"},
		},
		{
			name:  "start configure reader",
			steps: []string{"start", "configure", "reader"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := testOptions(tt.name)
			reader := &fakeReader{name: tt.name}
			verifier := &fakeVerifier{name: tt.name}
			called := make(chan struct{}, 1)
			manager := newTestManager(options, func(_ context.Context, gotReader client.Reader, gotOptions oidc.Options) (oidc.Verifier, error) {
				assert.Same(t, reader, gotReader)
				assert.Equal(t, options, gotOptions)
				called <- struct{}{}
				return verifier, nil
			})

			var cancel context.CancelFunc
			var done <-chan error
			for _, step := range tt.steps {
				switch step {
				case "configure":
					require.NoError(t, manager.Configure(true))
				case "reader":
					require.NoError(t, manager.SetReader(reader))
				case "start":
					cancel, done = startManager(t, manager)
				default:
					t.Fatalf("unknown startup step %q", step)
				}
			}

			select {
			case <-called:
			case <-time.After(testTimeout):
				t.Fatal("verifier loader was not called")
			}
			waitForState(t, manager, Ready)
			assert.Same(t, verifier, manager.Current())
			assert.NoError(t, manager.Ready())
			stopManager(t, cancel, done)
		})
	}
}

func TestManagerRetryAndPublication(t *testing.T) {
	tests := []struct {
		name          string
		failedResults []error
		expectFailure string
	}{
		{
			name:          "loader errors are retried",
			failedResults: []error{errors.New("discovery unavailable"), errors.New("JWKS unavailable")},
			expectFailure: "discovery unavailable",
		},
		{
			name:          "nil verifier is retried",
			failedResults: []error{nil},
			expectFailure: "nil verifier",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verifier := &fakeVerifier{name: tt.name}
			attempted := make(chan int, len(tt.failedResults)+1)
			var calls atomic.Int32
			manager := NewManagerWithDependencies(func() (oidc.Options, error) {
				return testOptions("retry"), nil
			}, func(context.Context, client.Reader, oidc.Options) (oidc.Verifier, error) {
				attempt := int(calls.Add(1))
				attempted <- attempt
				if attempt <= len(tt.failedResults) {
					return nil, tt.failedResults[attempt-1]
				}
				return verifier, nil
			}, 100*time.Millisecond, 100*time.Millisecond)
			require.NoError(t, manager.Configure(true))
			require.NoError(t, manager.SetReader(&fakeReader{name: "retry"}))
			cancel, done := startManager(t, manager)

			select {
			case <-attempted:
			case <-time.After(testTimeout):
				t.Fatal("first verifier attempt did not run")
			}
			require.Eventually(t, func() bool {
				err := manager.Ready()
				return err != nil && errors.Is(err, ErrInitializing) && strings.Contains(err.Error(), tt.expectFailure)
			}, testTimeout, time.Millisecond)

			waitForState(t, manager, Ready)
			assert.Same(t, verifier, manager.Current())
			expectCalls := int32(len(tt.failedResults) + 1)
			assert.Equal(t, expectCalls, calls.Load())
			time.Sleep(20 * time.Millisecond)
			assert.Equal(t, expectCalls, calls.Load(), "loader must not refresh after readiness")
			stopManager(t, cancel, done)
		})
	}
}

func TestManagerCurrentAtomicPublication(t *testing.T) {
	tests := []struct {
		name    string
		readers int
	}{
		{name: "concurrent readers observe publication", readers: 16},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verifier := &fakeVerifier{name: "published"}
			releaseLoader := make(chan struct{})
			manager := newTestManager(testOptions("atomic"), func(context.Context, client.Reader, oidc.Options) (oidc.Verifier, error) {
				<-releaseLoader
				return verifier, nil
			})
			require.NoError(t, manager.Configure(true))
			require.NoError(t, manager.SetReader(&fakeReader{name: "atomic"}))
			cancel, done := startManager(t, manager)

			results := make(chan oidc.Verifier, tt.readers)
			var readers sync.WaitGroup
			for range tt.readers {
				readers.Add(1)
				go func() {
					defer readers.Done()
					for {
						if current := manager.Current(); current != nil {
							results <- current
							return
						}
					}
				}()
			}
			close(releaseLoader)
			readers.Wait()
			close(results)
			for result := range results {
				assert.Same(t, verifier, result)
			}
			stopManager(t, cancel, done)
		})
	}
}

func TestManagerCancellationAndDisabledStart(t *testing.T) {
	tests := []struct {
		name        string
		prepare     func(*Manager, *atomic.Int32)
		expectCalls int32
	}{
		{
			name: "awaiting configuration",
			prepare: func(*Manager, *atomic.Int32) {
			},
		},
		{
			name: "enabled awaiting reader",
			prepare: func(manager *Manager, _ *atomic.Int32) {
				require.NoError(t, manager.Configure(true))
			},
		},
		{
			name: "disabled never loads",
			prepare: func(manager *Manager, _ *atomic.Int32) {
				require.NoError(t, manager.Configure(false))
				require.NoError(t, manager.SetReader(&fakeReader{name: "disabled"}))
			},
		},
		{
			name: "failed retry wait is cancellable",
			prepare: func(manager *Manager, calls *atomic.Int32) {
				require.NoError(t, manager.Configure(true))
				require.NoError(t, manager.SetReader(&fakeReader{name: "retry cancellation"}))
				require.Eventually(t, func() bool {
					return calls.Load() > 0
				}, testTimeout, time.Millisecond)
			},
			expectCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			manager := NewManagerWithDependencies(func() (oidc.Options, error) {
				return testOptions("cancellation"), nil
			}, func(context.Context, client.Reader, oidc.Options) (oidc.Verifier, error) {
				calls.Add(1)
				return nil, errors.New("still unavailable")
			}, time.Hour, time.Hour)
			cancel, done := startManager(t, manager)
			tt.prepare(manager, &calls)
			stopManager(t, cancel, done)
			assert.Equal(t, tt.expectCalls, calls.Load())
		})
	}
}

func TestManagerStartValidation(t *testing.T) {
	tests := []struct {
		name        string
		run         func(*Manager) error
		expectError string
	}{
		{
			name: "second start is rejected",
			run: func(manager *Manager) error {
				cancel, done := startManager(t, manager)
				defer stopManager(t, cancel, done)
				return manager.Start(context.Background())
			},
			expectError: "already started",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newTestManager(testOptions("start"), nil)
			err := tt.run(manager)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		name         string
		current      time.Duration
		maximum      time.Duration
		expectResult time.Duration
	}{
		{name: "doubles below cap", current: time.Second, maximum: 4 * time.Second, expectResult: 2 * time.Second},
		{name: "caps next interval", current: 3 * time.Second, maximum: 4 * time.Second, expectResult: 4 * time.Second},
		{name: "stays capped", current: 4 * time.Second, maximum: 4 * time.Second, expectResult: 4 * time.Second},
		{name: "zero remains zero", current: 0, maximum: 0, expectResult: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expectResult, nextBackoff(tt.current, tt.maximum), fmt.Sprintf("backoff for %s", tt.name))
		})
	}
}
