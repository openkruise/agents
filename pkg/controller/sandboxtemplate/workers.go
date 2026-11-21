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
	"errors"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/openkruise/agents/api/v1alpha1"
)

const (
	buildHeartbeatInterval = time.Minute
	buildLeaseDuration     = 3 * time.Minute
)

type buildWorker struct {
	key    client.ObjectKey
	cancel context.CancelFunc
	lease  *coordinationv1.Lease
}

func (r *Reconciler) NeedLeaderElection() bool { return false }

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

func (r *Reconciler) startWorker(version *api.SandboxTemplate, lease *coordinationv1.Lease) bool {
	r.workerMu.Lock()
	defer r.workerMu.Unlock()
	if r.workerCtx == nil || r.workerCtx.Err() != nil {
		return false
	}
	if r.workers[version.UID] != nil {
		return true
	}
	ctx, cancel := context.WithCancel(r.workerCtx)
	worker := &buildWorker{key: client.ObjectKeyFromObject(version), cancel: cancel, lease: lease.DeepCopy()}
	r.workers[version.UID] = worker
	r.workerWG.Add(1)
	version, lease = version.DeepCopy(), lease.DeepCopy()
	go func() {
		defer r.workerWG.Done()
		defer func() {
			r.workerMu.Lock()
			delete(r.workers, version.UID)
			r.workerMu.Unlock()
		}()
		heartbeatDone := make(chan struct{})
		heartbeatVersion := version.DeepCopy()
		go func() {
			defer close(heartbeatDone)
			r.keepBuildLease(ctx, heartbeatVersion, lease, cancel)
		}()
		defer func() {
			cancel()
			<-heartbeatDone
		}()
		if err := r.runExecution(ctx, version, lease); err != nil {
			logf.FromContext(ctx).Error(err, "persist build execution result", "version", worker.key)
		}
	}()
	return true
}

func buildLeaseKey(version *api.SandboxTemplate) client.ObjectKey {
	return client.ObjectKey{Namespace: version.Namespace, Name: "template-build-" + string(version.UID)}
}

func (r *Reconciler) buildLeaseActive(lease *coordinationv1.Lease) bool {
	return lease.Spec.RenewTime != nil && r.leaseClock.Now().Before(lease.Spec.RenewTime.Add(buildLeaseDuration))
}

// acquireBuildLease takes or renews the execution Lease for the Version.
// started reports that this build already published a BuildReady
// condition; an already-started build only renews its lease and never
// counts as a fresh acquisition.
func (r *Reconciler) acquireBuildLease(ctx context.Context, version *api.SandboxTemplate, started bool) (*coordinationv1.Lease, bool, error) {
	key := buildLeaseKey(version)
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, key, lease); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, false, fmt.Errorf("read build lease: %w", err)
		}
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: version.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptr.To(r.instanceID), LeaseDurationSeconds: ptr.To(int32(buildLeaseDuration / time.Second)), RenewTime: ptr.To(metav1.NewMicroTime(r.leaseClock.Now()))},
		}
		if err := r.Create(ctx, lease); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("create build lease: %w", err)
		}
		return lease, true, nil
	}
	if !ownedByVersion(lease, version) {
		return nil, false, fmt.Errorf("build lease ownership changed")
	}
	if r.buildLeaseActive(lease) {
		return lease, !started && ptr.Deref(lease.Spec.HolderIdentity, "") == r.instanceID, nil
	}
	lease.Spec.HolderIdentity = ptr.To(r.instanceID)
	lease.Spec.LeaseDurationSeconds = ptr.To(int32(buildLeaseDuration / time.Second))
	lease.Spec.RenewTime = ptr.To(metav1.NewMicroTime(r.leaseClock.Now()))
	if err := r.Update(ctx, lease); err != nil {
		if apierrors.IsConflict(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("acquire expired build lease: %w", err)
	}
	return lease, true, nil
}

func (r *Reconciler) confirmedBuildLease(version *api.SandboxTemplate, expected *coordinationv1.Lease) *coordinationv1.Lease {
	r.workerMu.Lock()
	defer r.workerMu.Unlock()
	if worker := r.workers[version.UID]; worker != nil && (expected == nil || worker.lease.UID == expected.UID && worker.lease.Spec.RenewTime.Time.After(expected.Spec.RenewTime.Time)) {
		return worker.lease.DeepCopy()
	}
	return expected.DeepCopy()
}

func (r *Reconciler) currentBuildLease(ctx context.Context, version *api.SandboxTemplate, expected *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	current := &api.SandboxTemplate{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(version), current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.Join(errExecutionInterrupted, err)
		}
		return nil, err
	}
	if current.UID != version.UID || current.Generation != version.Generation || current.DeletionTimestamp != nil || current.Status.Phase == api.SandboxTemplatePhaseFailed || current.Status.Phase == api.SandboxTemplatePhaseReady {
		return nil, errExecutionInterrupted
	}
	expected = r.confirmedBuildLease(version, expected)
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, buildLeaseKey(version), lease); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		lease = expected.DeepCopy()
	}
	if lease.UID != expected.UID {
		return nil, errExecutionInterrupted
	}
	// A cache older than a confirmed write cannot revoke that ownership.
	if lease.Spec.RenewTime == nil || lease.Spec.RenewTime.Time.Before(expected.Spec.RenewTime.Time) {
		lease = expected.DeepCopy()
	}
	if !ownedByVersion(lease, version) || ptr.Deref(lease.Spec.HolderIdentity, "") != r.instanceID || !r.buildLeaseActive(lease) {
		return nil, errExecutionInterrupted
	}
	return lease, nil
}

func (r *Reconciler) renewBuildLease(ctx context.Context, version *api.SandboxTemplate, expected *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	var renewed *coordinationv1.Lease
	err := wait.ExponentialBackoffWithContext(ctx, retry.DefaultBackoff, func(ctx context.Context) (bool, error) {
		lease, err := r.currentBuildLease(ctx, version, expected)
		if err != nil {
			return false, err
		}
		lease.Spec.RenewTime = ptr.To(metav1.NewMicroTime(r.leaseClock.Now()))
		if err := r.Update(ctx, lease); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			if apierrors.IsNotFound(err) {
				return false, errors.Join(errExecutionInterrupted, err)
			}
			return false, err
		}
		r.workerMu.Lock()
		if worker := r.workers[version.UID]; worker != nil && worker.lease.UID == lease.UID && !lease.Spec.RenewTime.Time.Before(worker.lease.Spec.RenewTime.Time) {
			worker.lease = lease.DeepCopy()
		}
		r.workerMu.Unlock()
		renewed = lease
		return true, nil
	})
	return renewed, err
}

func (r *Reconciler) keepBuildLease(ctx context.Context, version *api.SandboxTemplate, lease *coordinationv1.Lease, cancel context.CancelFunc) {
	ticker := r.leaseClock.NewTicker(buildHeartbeatInterval)
	defer ticker.Stop()
	timer := r.leaseClock.NewTimer(lease.Spec.RenewTime.Add(buildLeaseDuration).Sub(r.leaseClock.Now()))
	defer timer.Stop()
	results := make(chan struct {
		lease *coordinationv1.Lease
		err   error
	}, 1)
	var renewalDone chan struct{}
	var renewCancel context.CancelFunc
	defer func() {
		if renewCancel != nil {
			renewCancel()
			<-renewalDone
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
			r.workerMu.Lock()
			if worker := r.workers[version.UID]; worker != nil && worker.lease.UID == lease.UID && worker.lease.Spec.RenewTime.Time.After(lease.Spec.RenewTime.Time) {
				lease = worker.lease.DeepCopy()
			}
			remaining := lease.Spec.RenewTime.Add(buildLeaseDuration).Sub(r.leaseClock.Now())
			if remaining <= 0 {
				cancel()
			}
			r.workerMu.Unlock()
			if remaining <= 0 {
				return
			}
			timer.Reset(remaining)
		case <-ticker.C():
			if renewCancel != nil {
				continue
			}
			renewCtx, stop := context.WithTimeout(ctx, buildHeartbeatInterval/2)
			renewCancel = stop
			done := make(chan struct{})
			renewalDone = done
			// Renewal I/O must not block the ownership-expiry timer.
			go func(expected *coordinationv1.Lease) {
				defer close(done)
				defer stop()
				renewed, err := r.renewBuildLease(renewCtx, version, expected)
				select {
				case results <- struct {
					lease *coordinationv1.Lease
					err   error
				}{lease: renewed, err: err}:
				case <-ctx.Done():
				}
			}(lease)
		case result := <-results:
			<-renewalDone
			renewCancel = nil
			if ctx.Err() != nil {
				return
			}
			if result.err != nil {
				if errors.Is(result.err, errExecutionInterrupted) {
					cancel()
					return
				}
				logf.FromContext(ctx).Error(result.err, "renew build lease", "version", client.ObjectKeyFromObject(version))
				continue
			}
			lease = result.lease
			if !timer.Stop() {
				select {
				case <-timer.C():
				default:
				}
			}
			timer.Reset(lease.Spec.RenewTime.Add(buildLeaseDuration).Sub(r.leaseClock.Now()))
		}
	}
}

func (r *Reconciler) deleteBuildLease(ctx context.Context, version *api.SandboxTemplate) (bool, error) {
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, buildLeaseKey(version), lease); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !ownedByVersion(lease, version) {
		return false, fmt.Errorf("build lease ownership changed")
	}
	return r.requestDeletion(ctx, lease, metav1.DeletePropagationBackground)
}

func (r *Reconciler) stopWorkers(matches func(*buildWorker) bool) bool {
	r.workerMu.Lock()
	defer r.workerMu.Unlock()
	pending := false
	for _, worker := range r.workers {
		if matches(worker) {
			worker.cancel()
			pending = true
		}
	}
	return pending
}

func (r *Reconciler) stopVersionWorker(key client.ObjectKey) bool {
	return r.stopWorkers(func(worker *buildWorker) bool { return worker.key == key })
}
