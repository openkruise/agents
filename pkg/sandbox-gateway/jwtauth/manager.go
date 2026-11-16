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

// Package jwtauth manages process-wide initialization of the gateway JWT verifier.
package jwtauth

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openkruise/agents/pkg/identity/oidc"
)

const (
	defaultInitialBackoff = time.Second
	defaultMaxBackoff     = 30 * time.Second
)

var (
	// ErrAwaitingConfig indicates that Configure has not accepted a configuration yet.
	ErrAwaitingConfig = errors.New("JWT authentication is awaiting configuration")
	// ErrInitializing indicates that the manager has not published a verifier yet.
	ErrInitializing = errors.New("JWT authentication is initializing")
)

// State describes the manager's process-level lifecycle.
type State uint32

const (
	// AwaitingConfig is the initial state before Configure succeeds.
	AwaitingConfig State = iota
	// Initializing indicates that verifier initialization has not completed.
	Initializing
	// Ready indicates that a verifier has been published.
	Ready
)

// OptionsSource resolves and validates the local OIDC configuration.
type OptionsSource func() (oidc.Options, error)

// VerifierLoader constructs a verifier using Kubernetes-backed OIDC discovery data.
type VerifierLoader func(context.Context, client.Reader, oidc.Options) (oidc.Verifier, error)

type verifierHolder struct {
	verifier oidc.Verifier
}

// Manager coordinates one-time, process-level OIDC verifier initialization.
type Manager struct {
	optionsSource  OptionsSource
	loader         VerifierLoader
	initialBackoff time.Duration
	maxBackoff     time.Duration
	wake           chan struct{}

	state    atomic.Uint32
	verifier atomic.Pointer[verifierHolder]

	mu             sync.Mutex
	configured     bool
	sandboxEnabled bool
	reader         client.Reader
	started        bool
	lastFailure    error
}

// NewManager creates a manager using the production OIDC dependencies.
func NewManager() *Manager {
	return NewManagerWithDependencies(
		oidc.OptionsFromEnvironment,
		oidc.NewVerifier,
		defaultInitialBackoff,
		defaultMaxBackoff,
	)
}

// NewManagerWithDependencies creates a manager with injectable dependencies.
func NewManagerWithDependencies(
	optionsSource OptionsSource,
	loader VerifierLoader,
	initialBackoff, maxBackoff time.Duration,
) *Manager {
	if initialBackoff < 0 {
		initialBackoff = 0
	}
	if maxBackoff < 0 {
		maxBackoff = 0
	}
	if maxBackoff < initialBackoff {
		initialBackoff = maxBackoff
	}

	m := &Manager{
		optionsSource:  optionsSource,
		loader:         loader,
		initialBackoff: initialBackoff,
		maxBackoff:     maxBackoff,
		wake:           make(chan struct{}, 1),
	}
	m.state.Store(uint32(AwaitingConfig))
	return m
}

// Configure accepts the immutable ordinary Sandbox JWT switch. Verifier
// initialization runs in the background for both ordinary modes; local OIDC
// options failures never reject this switch or require another Configure call.
func (m *Manager) Configure(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.configured {
		if enabled != m.sandboxEnabled {
			return fmt.Errorf("JWT authentication is already configured with enabled=%t", m.sandboxEnabled)
		}
		return nil
	}
	m.configured = true
	m.sandboxEnabled = enabled
	m.state.Store(uint32(Initializing))
	m.notify()
	return nil
}

// SetReader provides the single Kubernetes reader used to construct the verifier.
func (m *Manager) SetReader(reader client.Reader) error {
	if isNilReader(reader) {
		return errors.New("JWT authentication reader must not be nil")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reader != nil {
		return errors.New("JWT authentication reader is already set")
	}
	m.reader = reader
	m.notify()
	return nil
}

// Start waits for configuration and a reader, then retries verifier construction.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("JWT authentication manager is already started")
	}
	m.started = true
	m.mu.Unlock()

	for {
		if ctx.Err() != nil {
			return nil
		}

		state, reader := m.startupInputs()
		switch state {
		case Ready:
			<-ctx.Done()
			return nil
		case Initializing:
			if reader != nil {
				return m.loadUntilReady(ctx, reader)
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-m.wake:
		}
	}
}

func (m *Manager) startupInputs() (State, client.Reader) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return State(m.state.Load()), m.reader
}

func (m *Manager) loadUntilReady(ctx context.Context, reader client.Reader) error {
	backoff := m.initialBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		if m.optionsSource == nil {
			m.recordFailure(ctx, errors.New("options source is nil"))
		} else if options, err := m.optionsSource(); err != nil {
			m.recordFailure(ctx, fmt.Errorf("resolve verifier options: %w", err))
		} else if m.loader == nil {
			m.recordFailure(ctx, errors.New("verifier loader is nil"))
		} else {
			verifier, err := m.loader(ctx, reader, options)
			if ctx.Err() != nil {
				return nil
			}
			if err == nil && !isNilVerifier(verifier) {
				m.verifier.Store(&verifierHolder{verifier: verifier})
				m.mu.Lock()
				m.lastFailure = nil
				m.state.Store(uint32(Ready))
				m.mu.Unlock()
				<-ctx.Done()
				return nil
			}
			if err == nil {
				err = errors.New("verifier loader returned a nil verifier")
			}
			m.recordFailure(ctx, err)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff = nextBackoff(backoff, m.maxBackoff)
	}
}

func (m *Manager) recordFailure(ctx context.Context, err error) {
	m.mu.Lock()
	m.lastFailure = err
	m.mu.Unlock()
	log.FromContext(ctx).Error(err, "failed to initialize traffic access token JWT verifier")
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum-current {
		return maximum
	}
	return current * 2
}

func (m *Manager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// NeedLeaderElection declares that verifier initialization runs in every process.
func (m *Manager) NeedLeaderElection() bool {
	return false
}

// Current returns the published verifier without locking or blocking.
func (m *Manager) Current() oidc.Verifier {
	holder := m.verifier.Load()
	if holder == nil {
		return nil
	}
	return holder.verifier
}

// Ready gates ordinary Sandbox JWT authentication only. SessionService callers use
// Current and remain unavailable until the verifier has been published.
func (m *Manager) Ready() error {
	m.mu.Lock()
	ordinaryDisabled := m.configured && !m.sandboxEnabled
	state := State(m.state.Load())
	lastFailure := m.lastFailure
	m.mu.Unlock()
	if ordinaryDisabled {
		return nil
	}
	switch state {
	case Ready:
		return nil
	case AwaitingConfig:
		return ErrAwaitingConfig
	case Initializing:
		if lastFailure != nil {
			return fmt.Errorf("%w: last verifier initialization failed: %w", ErrInitializing, lastFailure)
		}
		return ErrInitializing
	default:
		return fmt.Errorf("JWT authentication has unknown state %d", state)
	}
}

// State returns the manager's current lifecycle state.
func (m *Manager) State() State {
	return State(m.state.Load())
}

func isNilReader(reader client.Reader) bool {
	return isNilInterface(reader)
}

func isNilVerifier(verifier oidc.Verifier) bool {
	return isNilInterface(verifier)
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
