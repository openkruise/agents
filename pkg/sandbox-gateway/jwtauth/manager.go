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

// Package jwtauth manages process-wide initialization and refresh of the
// gateway JWT verifier.
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
	// defaultRefreshInterval is how often a published verifier is rebuilt from
	// the identity provider. A verifier holds an immutable JWKS snapshot, so
	// without periodic refresh every token signed with a rotated key is
	// rejected as an unknown kid until the process restarts.
	defaultRefreshInterval = 10 * time.Minute
)

var (
	// ErrAwaitingConfig indicates that Configure has not accepted a configuration yet.
	ErrAwaitingConfig = errors.New("JWT authentication is awaiting configuration")
	// ErrInitializing indicates that an enabled manager has not published a verifier yet.
	ErrInitializing = errors.New("JWT authentication is initializing")
)

// State describes the manager's process-level lifecycle.
type State uint32

const (
	// AwaitingConfig is the initial state before Configure succeeds.
	AwaitingConfig State = iota
	// Disabled indicates that JWT authentication was explicitly disabled.
	Disabled
	// Initializing indicates that JWT authentication is enabled but has no verifier yet.
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

// Manager coordinates process-level OIDC verifier initialization and keeps the
// published verifier refreshed so that provider key rotation is picked up.
type Manager struct {
	optionsSource   OptionsSource
	loader          VerifierLoader
	initialBackoff  time.Duration
	maxBackoff      time.Duration
	refreshInterval time.Duration
	wake            chan struct{}

	state    atomic.Uint32
	verifier atomic.Pointer[verifierHolder]

	mu          sync.Mutex
	configured  bool
	options     oidc.Options
	reader      client.Reader
	started     bool
	lastFailure error
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

// NewManagerWithDependencies creates a manager with injectable dependencies and
// the default verifier refresh interval.
func NewManagerWithDependencies(
	optionsSource OptionsSource,
	loader VerifierLoader,
	initialBackoff, maxBackoff time.Duration,
) *Manager {
	return newManager(optionsSource, loader, initialBackoff, maxBackoff, defaultRefreshInterval)
}

// newManager additionally takes the refresh interval so tests can exercise the
// refresh loop without waiting for the production interval. A non-positive
// interval disables refresh and keeps the first published verifier forever.
func newManager(
	optionsSource OptionsSource,
	loader VerifierLoader,
	initialBackoff, maxBackoff, refreshInterval time.Duration,
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
		optionsSource:   optionsSource,
		loader:          loader,
		initialBackoff:  initialBackoff,
		maxBackoff:      maxBackoff,
		refreshInterval: refreshInterval,
		wake:            make(chan struct{}, 1),
	}
	m.state.Store(uint32(AwaitingConfig))
	return m
}

// Configure selects the manager's immutable enabled or disabled mode.
// Enabling only resolves local options; verifier construction happens in Start.
func (m *Manager) Configure(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.configured {
		return m.validateRepeatedConfiguration(enabled)
	}

	if !enabled {
		m.configured = true
		m.state.Store(uint32(Disabled))
		m.notify()
		return nil
	}
	if m.optionsSource == nil {
		return errors.New("configure JWT authentication: options source is nil")
	}

	options, err := m.optionsSource()
	if err != nil {
		return fmt.Errorf("configure JWT authentication: %w", err)
	}
	m.options = options
	m.configured = true
	m.state.Store(uint32(Initializing))
	m.notify()
	return nil
}

func (m *Manager) validateRepeatedConfiguration(enabled bool) error {
	configuredEnabled := State(m.state.Load()) != Disabled
	if enabled != configuredEnabled {
		return fmt.Errorf("JWT authentication is already configured with enabled=%t", configuredEnabled)
	}
	if !enabled {
		return nil
	}
	options, err := m.optionsSource()
	if err != nil {
		return fmt.Errorf("reconfigure JWT authentication: %w", err)
	}
	if !reflect.DeepEqual(options, m.options) {
		return errors.New("JWT authentication is already configured with different options")
	}
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

// Start waits for configuration and a reader, retries verifier construction,
// then keeps the published verifier refreshed until the context is canceled.
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

		state, reader, options := m.startupInputs()
		switch state {
		case Disabled, Ready:
			<-ctx.Done()
			return nil
		case Initializing:
			if reader != nil {
				return m.loadUntilReady(ctx, reader, options)
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-m.wake:
		}
	}
}

func (m *Manager) startupInputs() (State, client.Reader, oidc.Options) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return State(m.state.Load()), m.reader, m.options
}

func (m *Manager) loadUntilReady(ctx context.Context, reader client.Reader, options oidc.Options) error {
	backoff := m.initialBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		verifier, err := m.load(ctx, reader, options)
		if err == nil {
			m.publish(verifier)
			return m.refreshUntilDone(ctx, reader, options)
		}
		m.recordFailure(ctx, err, "failed to initialize traffic access token JWT verifier")

		if !m.sleep(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff, m.maxBackoff)
	}
}

// refreshUntilDone rebuilds the verifier every refresh interval so that a
// rotated provider signing key is picked up without restarting the process.
// A failed refresh keeps the last published verifier in place and is retried
// with backoff, because stale keys still verify unrotated tokens.
func (m *Manager) refreshUntilDone(ctx context.Context, reader client.Reader, options oidc.Options) error {
	if m.refreshInterval <= 0 {
		<-ctx.Done()
		return nil
	}

	backoff := m.initialBackoff
	wait := m.refreshInterval
	for {
		if !m.sleep(ctx, wait) {
			return nil
		}

		verifier, err := m.load(ctx, reader, options)
		if err != nil {
			m.recordFailure(ctx, err, "failed to refresh traffic access token JWT verifier, keeping the current one")
			// Retry sooner than the refresh interval while the provider is
			// unavailable, but never spin when no backoff is configured.
			wait = backoff
			if wait <= 0 {
				wait = m.refreshInterval
			}
			backoff = nextBackoff(backoff, m.maxBackoff)
			continue
		}

		m.publish(verifier)
		backoff = m.initialBackoff
		wait = m.refreshInterval
	}
}

// load builds a verifier and normalizes a nil verifier into an error.
func (m *Manager) load(ctx context.Context, reader client.Reader, options oidc.Options) (oidc.Verifier, error) {
	if m.loader == nil {
		return nil, errors.New("verifier loader is nil")
	}
	verifier, err := m.loader(ctx, reader, options)
	if err != nil {
		return nil, err
	}
	if isNilVerifier(verifier) {
		return nil, errors.New("verifier loader returned a nil verifier")
	}
	return verifier, nil
}

// publish installs the verifier for readers and marks the manager ready.
func (m *Manager) publish(verifier oidc.Verifier) {
	m.verifier.Store(&verifierHolder{verifier: verifier})
	m.mu.Lock()
	m.lastFailure = nil
	m.state.Store(uint32(Ready))
	m.mu.Unlock()
}

// sleep waits for the duration and reports whether it elapsed rather than the
// context being canceled.
func (m *Manager) sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	select {
	case <-ctx.Done():
		timer.Stop()
		return false
	case <-timer.C:
		return true
	}
}

func (m *Manager) recordFailure(ctx context.Context, err error, message string) {
	m.mu.Lock()
	m.lastFailure = err
	m.mu.Unlock()
	log.FromContext(ctx).Error(err, message)
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

// Ready reports whether JWT authentication is usable for the accepted mode.
func (m *Manager) Ready() error {
	state := State(m.state.Load())
	switch state {
	case Disabled, Ready:
		return nil
	case AwaitingConfig:
		return ErrAwaitingConfig
	case Initializing:
		m.mu.Lock()
		lastFailure := m.lastFailure
		m.mu.Unlock()
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
