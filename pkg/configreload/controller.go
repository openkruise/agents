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

// Package configreload observes a small, explicitly assembled set of runtime inputs.
package configreload

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Component declares ownership explicitly; the dispatcher has no business policy.
type Component struct {
	Name string
	Keys []string
	// Reload applies one increment of owned keys as a single atomic unit: the
	// returned Result covers every key, partial application must not be reported
	// as Accepted, and independently effective keys belong to separate Components.
	Reload func(context.Context, map[string]string) (Result, error)
}

type componentState struct {
	Component
	confirmed map[string]string
	pending   map[string]bool
}

// Dispatcher is owned by one serial reconciler. Failed writes retain their key
// intent even when the latest target returns to the last confirmed input.
type Dispatcher struct {
	components  []componentState
	initialized chan struct{}
}

func NewDispatcher(components []Component) *Dispatcher {
	d := &Dispatcher{initialized: make(chan struct{})}
	for _, c := range components {
		d.components = append(d.components, componentState{Component: c, confirmed: map[string]string{}, pending: map[string]bool{}})
	}
	return d
}

// Apply distributes one input snapshot to every component. Component state is
// unlocked, so concurrent calls are unsupported: a single owner must call it.
func (d *Dispatcher) Apply(ctx context.Context, values map[string]string) error {
	log := klog.FromContext(ctx)
	var failed bool
	for i := range d.components {
		c := &d.components[i]
		changes := map[string]string{}
		for _, key := range c.Keys {
			old, ok := c.confirmed[key]
			if !ok || old != values[key] || c.pending[key] {
				changes[key] = values[key]
			}
		}
		if len(changes) == 0 {
			continue
		}
		keys := slices.Sorted(maps.Keys(changes))
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		result, err := c.Reload(attempt, changes)
		cancel()
		if err != nil {
			for key := range changes {
				c.pending[key] = true
			}
			failed = true
			// A backend error can contain credentials. Only expose a fixed category.
			log.Info("configuration reload incomplete", "component", c.Name, "keys", keys, "category", "backend")
			continue
		}
		switch result {
		case Accepted:
			log.Info("configuration applied", "component", c.Name, "keys", keys)
			for key, value := range changes {
				c.confirmed[key] = value
				delete(c.pending, key)
			}
		case NotNecessary:
			log.Info("configuration reload not necessary", "component", c.Name, "keys", keys)
			for key, value := range changes {
				c.confirmed[key] = value
				delete(c.pending, key)
			}
		case Rejected:
			log.Info("configuration rejected", "component", c.Name, "keys", keys)
		default:
			log.Info("configuration reload returned an unknown result", "component", c.Name, "result", string(result), "keys", keys)
			failed = true
			for key := range changes {
				c.pending[key] = true
			}
		}
	}
	d.notifyInitialized()
	if failed {
		return errors.New("configuration reload incomplete")
	}
	return nil
}

// notifyInitialized releases startup after the first confirmation of every owned key.
func (d *Dispatcher) notifyInitialized() {
	select {
	case <-d.initialized:
		return
	default:
	}
	for _, c := range d.components {
		for _, key := range c.Keys {
			if _, ok := c.confirmed[key]; !ok {
				return
			}
		}
	}
	close(d.initialized)
}

// Wait requires each component's first confirmed configuration. Initial rejection
// or failure must not release startup; even no-op components need a valid snapshot.
func (d *Dispatcher) Wait(ctx context.Context) error {
	select {
	case <-d.initialized:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type secretReconciler struct {
	reader     client.Reader
	ref        types.NamespacedName
	parse      func(map[string][]byte) (map[string]string, error)
	dispatcher *Dispatcher
}

func (r *secretReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	secret := &corev1.Secret{}
	if err := r.reader.Get(ctx, r.ref, secret); err != nil {
		if apierrors.IsNotFound(err) {
			klog.FromContext(ctx).Info("configuration Secret is absent; the current configuration remains in effect", "secret", r.ref)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, errors.New("configuration Secret unavailable")
	}
	values, err := r.parse(secret.Data)
	if err != nil {
		klog.FromContext(ctx).Info("configuration Secret has invalid shape", "secret", r.ref, "version", secret.ResourceVersion)
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.dispatcher.Apply(ctx, values)
}

// Watcher owns a dedicated, exactly scoped informer. Its lifetime includes Init
// waits and steady state and never depends on the sandbox cache startup.
type Watcher struct{ manager ctrl.Manager }

func NewWatcher(ctx context.Context, cfg *rest.Config, ref types.NamespacedName, parse func(map[string][]byte) (map[string]string, error), d *Dispatcher) (*Watcher, error) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Logger: klog.FromContext(ctx),
		Scheme: scheme, LeaderElection: false, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{ref.Namespace: {}}, ByObject: map[client.Object]cache.ByObject{&corev1.Secret{}: {Field: fields.OneTermEqualSelector("metadata.name", ref.Name)}}},
	})
	if err != nil {
		return nil, err
	}
	r := &secretReconciler{reader: mgr.GetCache(), ref: ref, parse: parse, dispatcher: d}
	// Apply is not concurrency-safe; never raise this above one.
	if err := ctrl.NewControllerManagedBy(mgr).Named("runtime-config").For(&corev1.Secret{}).WithOptions(controller.Options{MaxConcurrentReconciles: 1, SkipNameValidation: boolPointer(true)}).Complete(r); err != nil {
		return nil, err
	}
	return &Watcher{manager: mgr}, nil
}

func boolPointer(v bool) *bool { return &v }

func (w *Watcher) Start(ctx context.Context) error { return w.manager.Start(ctx) }

// RunStatic initializes flag/env inputs and retries incomplete effects without
// creating a Secret observer. Configuration and store observation remain separate.
func RunStatic(ctx context.Context, d *Dispatcher, values map[string]string) error {
	for {
		if err := d.Apply(ctx, values); err == nil {
			<-ctx.Done()
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
