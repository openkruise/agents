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

package sandboxtemplate

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/openkruise/agents/api/v1alpha1"
)

// buildWorkerPollInterval is how often the reconciler re-checks a build that
// has a live worker, so it notices worker loss and deadline expiry.
const buildWorkerPollInterval = 30 * time.Second

type buildWorker struct {
	key    client.ObjectKey
	cancel context.CancelFunc
}

// NeedLeaderElection keeps the worker registry on the elected leader, so a
// build runs on one replica at a time and a leadership change is the only
// handoff point.
func (r *Reconciler) NeedLeaderElection() bool { return true }

func (r *Reconciler) Start(ctx context.Context) error {
	r.workerMu.Lock()
	r.workerCtx = ctx
	r.workers = make(map[types.UID]*buildWorker)
	r.workerMu.Unlock()
	<-ctx.Done()
	r.workerMu.Lock()
	r.workerCtx = nil
	for _, worker := range r.workers {
		worker.cancel()
	}
	r.workerMu.Unlock()
	r.workerWG.Wait()
	return nil
}

func (r *Reconciler) hasWorker(uid types.UID) bool {
	r.workerMu.Lock()
	defer r.workerMu.Unlock()
	return r.workers[uid] != nil
}

func (r *Reconciler) startWorker(version *api.SandboxTemplate) bool {
	r.workerMu.Lock()
	defer r.workerMu.Unlock()
	if r.workerCtx == nil || r.workerCtx.Err() != nil {
		return false
	}
	if r.workers[version.UID] != nil {
		return true
	}
	ctx, cancel := context.WithCancel(r.workerCtx)
	worker := &buildWorker{key: client.ObjectKeyFromObject(version), cancel: cancel}
	r.workers[version.UID] = worker
	r.workerWG.Add(1)
	version = version.DeepCopy()
	go func() {
		defer r.workerWG.Done()
		defer func() {
			r.workerMu.Lock()
			delete(r.workers, version.UID)
			r.workerMu.Unlock()
		}()
		if err := r.runExecution(ctx, version); err != nil {
			logf.FromContext(ctx).Error(err, "persist build execution result", "version", worker.key)
		}
	}()
	return true
}

func (r *Reconciler) stopVersionWorker(key client.ObjectKey) bool {
	r.workerMu.Lock()
	defer r.workerMu.Unlock()
	pending := false
	for _, worker := range r.workers {
		if worker.key == key {
			worker.cancel()
			pending = true
		}
	}
	return pending
}
