/*
Copyright 2025.

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

package controllers

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// MockManager implements manager.Manager for unit testing.
// It provides controllable Add() behavior to allow error injection on specific calls.
type MockManager struct {
	client       client.Client
	apiReader    client.Reader
	scheme       *runtime.Scheme
	addCallCount atomic.Int32
	startCount   atomic.Int32
	// failOnNthAdd specifies which (1-based) call to Add() should return addError.
	// 0 means never fail.
	failOnNthAdd int
	addError     error
	// Wait simulation fields
	waitSimEnabled    bool
	waitHooks         *sync.Map
	waitReconcilers   map[reflect.Type]reconcile.Reconciler
	waitMu            sync.RWMutex
	waitReconcileKeys map[reflect.Type][]ctrl.Request
}

type MockManagerBuilder struct {
	mgr *MockManager
	t   *testing.T
}

func NewMockManagerBuilder(t *testing.T) (*MockManagerBuilder, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add agents/v1alpha1 to scheme: %w", err)
	}
	return &MockManagerBuilder{
		mgr: &MockManager{
			client: fake.NewClientBuilder().WithScheme(scheme).Build(), // overwritable via WithClient
			scheme: scheme,
		},
		t: t,
	}, nil
}

func (b *MockManagerBuilder) WithClient(c client.Client) *MockManagerBuilder {
	b.t.Helper()
	b.mgr.client = c
	b.mgr.apiReader = c // fake client also implements Reader interface
	return b
}

func (b *MockManagerBuilder) WithScheme(s *runtime.Scheme) *MockManagerBuilder {
	b.t.Helper()
	b.mgr.scheme = s
	return b
}

// WithFailOnNthAdd specifies which (1-based) call to Add() should return addError.
func (b *MockManagerBuilder) WithFailOnNthAdd(n int, err error) *MockManagerBuilder {
	b.t.Helper()
	b.mgr.failOnNthAdd = n
	b.mgr.addError = err
	return b
}

// WithWaitSimulation enables WaitReconciler simulation on MockManager.
// Optionally accepts initial objects whose keys will be periodically reconciled.
func (b *MockManagerBuilder) WithWaitSimulation(objs ...client.Object) *MockManagerBuilder {
	b.t.Helper()
	b.mgr.waitSimEnabled = true
	for _, obj := range objs {
		b.mgr.AddWaitReconcileKey(obj)
	}
	return b
}

func (b *MockManagerBuilder) Build() *MockManager {
	b.t.Helper()
	return b.mgr
}

// --- cluster.Cluster interface ---

func (m *MockManager) GetHTTPClient() *http.Client          { return nil }
func (m *MockManager) GetConfig() *rest.Config              { return nil }
func (m *MockManager) GetCache() ctrlcache.Cache            { return nil }
func (m *MockManager) GetScheme() *runtime.Scheme           { return m.scheme }
func (m *MockManager) GetClient() client.Client             { return m.client }
func (m *MockManager) GetAPIReader() client.Reader          { return m.apiReader }
func (m *MockManager) GetFieldIndexer() client.FieldIndexer { return nil }
func (m *MockManager) GetRESTMapper() apimeta.RESTMapper    { return nil }

func (m *MockManager) GetEventRecorderFor(_ string) record.EventRecorder { return nil }

// Start satisfies both cluster.Cluster and manager.Manager.
func (m *MockManager) Start(ctx context.Context) error {
	m.startCount.Add(1)
	if m.waitSimEnabled && m.waitHooks != nil {
		m.initWaitReconcilers()
		go m.runWaitSimulation(ctx)
	}
	return nil
}

// --- manager.Manager interface ---

// Add records each call and returns addError on the configured nth invocation.
func (m *MockManager) Add(manager.Runnable) error {
	n := int(m.addCallCount.Add(1))
	if m.failOnNthAdd > 0 && n == m.failOnNthAdd {
		return m.addError
	}
	return nil
}

// SetWaitHooks sets the shared waitHooks reference from CacheV2.
// Must be called before Start().
func (m *MockManager) SetWaitHooks(hooks *sync.Map) {
	m.waitHooks = hooks
}

// initWaitReconcilers creates WaitReconciler instances for each supported object type,
// sharing the same waitHooks with CacheV2 to properly trigger wait entry resolution.
func (m *MockManager) initWaitReconcilers() {
	m.waitReconcilers = map[reflect.Type]reconcile.Reconciler{
		reflect.TypeOf(&agentsv1alpha1.Sandbox{}): &CacheSandboxWaitReconciler{
			WaitReconciler: WaitReconciler[*agentsv1alpha1.Sandbox]{
				Client:    m.client,
				Scheme:    m.scheme,
				waitHooks: m.waitHooks,
				NewObject: NewSandbox,
				Name:      "SandboxWaitSim",
			},
		},
		reflect.TypeOf(&agentsv1alpha1.Checkpoint{}): &CacheCheckpointWaitReconciler{
			WaitReconciler: WaitReconciler[*agentsv1alpha1.Checkpoint]{
				Client:    m.client,
				Scheme:    m.scheme,
				waitHooks: m.waitHooks,
				NewObject: NewCheckpoint,
				Name:      "CheckpointWaitSim",
			},
		},
	}
}

func (m *MockManager) runWaitSimulation(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.waitMu.RLock()
			snapshot := make(map[reflect.Type][]ctrl.Request, len(m.waitReconcileKeys))
			for t, ks := range m.waitReconcileKeys {
				snapshot[t] = append([]ctrl.Request(nil), ks...)
			}
			m.waitMu.RUnlock()

			for objType, keys := range snapshot {
				r, ok := m.waitReconcilers[objType]
				if !ok {
					continue
				}
				for _, req := range keys {
					_, _ = r.Reconcile(ctx, req)
				}
			}
		}
	}
}

// AddWaitReconcileKey registers an object key for periodic wait reconciliation.
// The key is grouped by object type so only the matching WaitReconciler is invoked.
func (m *MockManager) AddWaitReconcileKey(obj client.Object) {
	objType := reflect.TypeOf(obj)
	m.waitMu.Lock()
	defer m.waitMu.Unlock()
	if m.waitReconcileKeys == nil {
		m.waitReconcileKeys = make(map[reflect.Type][]ctrl.Request)
	}
	m.waitReconcileKeys[objType] = append(m.waitReconcileKeys[objType], ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
		},
	})
}

func (m *MockManager) Elected() <-chan struct{} { return nil }

func (m *MockManager) AddMetricsServerExtraHandler(_ string, _ http.Handler) error { return nil }
func (m *MockManager) AddHealthzCheck(_ string, _ healthz.Checker) error           { return nil }
func (m *MockManager) AddReadyzCheck(_ string, _ healthz.Checker) error            { return nil }

func (m *MockManager) GetWebhookServer() webhook.Server { return nil }
func (m *MockManager) GetLogger() logr.Logger           { return logr.Discard() }

// GetControllerOptions returns a config with SkipNameValidation=true.
// This prevents the global name registry inside controller-runtime from causing
// "controller already exists" failures across test cases that each call
// SetupCacheControllersWithManager (which registers the same 4 controller names).
func (m *MockManager) GetControllerOptions() ctrlcfg.Controller {
	skip := true
	return ctrlcfg.Controller{SkipNameValidation: &skip}
}

// addCallsCount returns how many times Add() was invoked.
func (m *MockManager) addCallsCount() int {
	return int(m.addCallCount.Load())
}

func (m *MockManager) StartCallsCount() int {
	return int(m.startCount.Load())
}

var _ ctrl.Manager = (*MockManager)(nil)
