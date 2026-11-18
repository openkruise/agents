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
	"fmt"

	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandboxid"
	session "github.com/openkruise/agents/pkg/sessionservice"
)

const sessionServiceWorkerPrefix = "sandbox-manager-session-service-id-worker"

func (m *SandboxManager) initializeSessionServiceRuntime(ctx context.Context) error {
	backend := m.sessionBackend()
	if backend == nil {
		return nil
	}
	m.servingBackend = backend
	worker, err := m.allocateWorkerID(ctx, sessionServiceWorkerPrefix)
	if err != nil {
		return fmt.Errorf("allocate SessionService ID worker: %w", err)
	}
	generate, err := sandboxid.NewGenerator(worker)
	if err != nil {
		return err
	}
	m.generateSessionServiceID = generate
	return nil
}

type sessionServiceIdentityKey struct{ namespace, name, uid string }

// startSessionServiceIdentityWorker subscribes once to the scoped cached replay and
// updates. Informer callbacks only enqueue; a single worker owns retry policy.
func (m *SandboxManager) startSessionServiceIdentityWorker(ctx context.Context, backend session.SessionServiceSessionBackend) error {
	source, ok := backend.(session.SessionServiceIdentitySource)
	if !ok {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[sessionServiceIdentityKey]())
	remove, err := source.SubscribeSessionServiceIdentities(ctx, func(observed session.SessionService) {
		if observed.ID == "" && !observed.Deleting {
			queue.Add(sessionServiceIdentityKey{observed.Namespace, observed.Name, observed.UID})
		}
	})
	if err != nil {
		queue.ShutDown()
		return fmt.Errorf("subscribe SessionService identity events: %w", err)
	}
	done := make(chan struct{})
	m.sessionServiceIdentityDone = done
	stopShutdown := context.AfterFunc(ctx, queue.ShutDown)
	go func() {
		defer close(done)
		defer stopShutdown()
		defer queue.ShutDown()
		defer func() {
			if err := remove(); err != nil {
				klog.FromContext(ctx).Error(err, "remove SessionService identity event registration")
			}
		}()
		for {
			key, shutdown := queue.Get()
			if shutdown {
				return
			}
			if ctx.Err() != nil {
				queue.Forget(key)
				queue.Done(key)
				return
			}
			err := m.assignSessionServiceIdentity(ctx, backend, source, key)
			if err != nil && ctx.Err() == nil && !errors.Is(err, session.ErrSessionServiceNotFound) {
				klog.FromContext(ctx).Error(err, "assign SessionService identity", "namespace", key.namespace, "name", key.name)
				queue.AddRateLimited(key)
			} else {
				queue.Forget(key)
			}
			queue.Done(key)
		}
	}()
	return nil
}

func (m *SandboxManager) assignSessionServiceIdentity(ctx context.Context, backend session.SessionServiceSessionBackend, source session.SessionServiceIdentitySource, key sessionServiceIdentityKey) error {
	parent, err := source.ReadSessionServiceIdentity(ctx, session.SessionService{Namespace: key.namespace, Name: key.name, UID: key.uid})
	if err != nil {
		return err
	}
	if parent.ID != "" || parent.Deleting {
		return nil
	}
	if m.generateSessionServiceID == nil {
		return fmt.Errorf("SessionService ID generator is not initialized")
	}
	id, err := m.generateSessionServiceID()
	if err != nil {
		return err
	}
	_, err = backend.AssignSessionServiceID(ctx, parent, id)
	return err
}
