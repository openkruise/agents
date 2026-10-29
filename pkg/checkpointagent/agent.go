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

// Package checkpointagent implements the node-local executor of the Checkpoint
// API. One instance runs per node and only ever touches Checkpoints whose
// target Pod is on that node: it dumps the Pod through the CRI CheckpointPod
// RPC into a directory on the node's own disk and records that directory in
// the Checkpoint status as a Local checkpoint source. The artifacts stay on
// the node, so the Sandbox controller pins the restored Pod back to it.
package checkpointagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	checkpointutils "github.com/openkruise/agents/pkg/controller/checkpoint"
	"github.com/openkruise/agents/pkg/utils"
)

// DefaultCheckpointRoot is the directory the agent writes checkpoints into.
// It is a node-local path on purpose: the whole point of this agent is to keep
// the artifacts next to the runtime that produced them.
const DefaultCheckpointRoot = "/var/lib/kruise-agents/checkpoints"

// DefaultCheckpointTimeout bounds a single CheckpointPod RPC. A dump writes
// the guest memory to disk, so it is minutes rather than seconds, but it must
// not be unbounded: a runtime that never returns would hold the Checkpoint in
// Creating forever and with it the sandbox pause that is waiting on it.
const DefaultCheckpointTimeout = 10 * time.Minute

// Options configures the node checkpoint agent.
type Options struct {
	// NodeName is the node this agent runs on. It is recorded in the
	// Checkpoint status so the restore path can find the artifacts again.
	NodeName string
	// CRIEndpoint is the unix socket of the node's container runtime.
	CRIEndpoint string
	// CheckpointRoot is the parent directory of all checkpoint directories.
	CheckpointRoot string
	// RuntimeClasses restricts the agent to Pods using one of these runtime
	// classes. Empty means every Pod on the node is eligible, which is only
	// safe when every runtime on the node implements CheckpointPod.
	RuntimeClasses []string
	// CheckpointTimeout bounds a single CheckpointPod RPC.
	CheckpointTimeout time.Duration
}

// Reconciler executes Checkpoints for the Pods of a single node.
type Reconciler struct {
	client.Client
	runtime runtimeService
	opts    Options
}

// Add wires the node checkpoint agent into mgr. The returned error is fatal:
// without a runtime connection the agent would accept Checkpoints it can
// never execute.
func Add(mgr ctrl.Manager, opts Options) error {
	if opts.NodeName == "" {
		return fmt.Errorf("node name is required")
	}
	if opts.CheckpointRoot == "" {
		opts.CheckpointRoot = DefaultCheckpointRoot
	}
	if opts.CheckpointTimeout <= 0 {
		opts.CheckpointTimeout = DefaultCheckpointTimeout
	}
	svc, conn, err := dialRuntimeService(opts.CRIEndpoint)
	if err != nil {
		return err
	}
	if err := mgr.Add(closeOnStop{conn}); err != nil {
		return err
	}
	if err := os.MkdirAll(opts.CheckpointRoot, 0o700); err != nil {
		return fmt.Errorf("failed to create checkpoint root %q: %w", opts.CheckpointRoot, err)
	}
	r := &Reconciler{Client: mgr.GetClient(), runtime: svc, opts: opts}
	return ctrl.NewControllerManagedBy(mgr).
		Named("node-checkpoint-agent").
		For(&agentsv1alpha1.Checkpoint{}).
		Complete(r)
}

// Reconcile drives one Checkpoint through claim -> dump -> terminal phase, and
// removes the artifacts when the Checkpoint is deleted. Checkpoints targeting
// another node are left untouched: the Pod cache is restricted to this node,
// so they simply do not resolve to a Pod here.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cp := &agentsv1alpha1.Checkpoint{}
	if err := r.Get(ctx, req.NamespacedName, cp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cp.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDeleting(ctx, cp)
	}

	switch cp.Status.Phase {
	case agentsv1alpha1.CheckpointSucceeded, agentsv1alpha1.CheckpointFailed:
		// Terminal: the artifacts are kept until the Checkpoint is deleted,
		// which is what the finalizer path below reclaims.
		return ctrl.Result{}, nil
	case agentsv1alpha1.CheckpointCreating:
		if !r.ownsCheckpoint(cp) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.executeCheckpoint(ctx, cp)
	default:
		return ctrl.Result{}, r.claimCheckpoint(ctx, cp)
	}
}

// claimCheckpoint decides whether this node executes the Checkpoint and, if
// so, publishes the destination directory before any dump happens. Recording
// the location first is what makes the execution recoverable: an agent that
// restarts mid-dump finds the Checkpoint in Creating with its own node in the
// location and resumes from there, whereas a dump that ran before the
// location was published could leave artifacts nobody owns.
func (r *Reconciler) claimCheckpoint(ctx context.Context, cp *agentsv1alpha1.Checkpoint) error {
	log := logf.FromContext(ctx)
	pod, err := r.resolvePod(ctx, cp)
	if err != nil {
		return err
	}
	if pod == nil {
		log.V(4).Info("checkpoint target pod is not on this node, skipping", "checkpoint", klogName(cp))
		return nil
	}
	if !r.runtimeClassEligible(pod) {
		log.V(4).Info("checkpoint target pod runtime class is not checkpointable, skipping",
			"checkpoint", klogName(cp), "pod", pod.Name, "runtimeClass", pod.Spec.RuntimeClassName)
		return nil
	}

	// A pod-info-only checkpoint records no container state, so there is
	// nothing to dump, nothing on disk to reclaim, and no finalizer to hold.
	if agentsv1alpha1.IsPodInfoOnlyCheckpoint(cp) {
		return r.markSucceeded(ctx, cp, pod)
	}

	path, err := r.checkpointPath(checkpointIDFor(cp))
	if err != nil {
		return r.markFailed(ctx, cp, err)
	}
	cp.Status.Phase = agentsv1alpha1.CheckpointCreating
	cp.Status.CheckpointId = checkpointIDFor(cp)
	cp.Status.CheckpointLocation = &agentsv1alpha1.CheckpointSource{
		Type:  agentsv1alpha1.CheckpointSourceTypeLocal,
		Local: &agentsv1alpha1.LocalCheckpointSource{NodeName: r.opts.NodeName, Path: path},
	}
	cp.Status.Message = fmt.Sprintf("Checkpointing pod %s on node %s", pod.Name, r.opts.NodeName)
	cp.Status.ObservedGeneration = cp.Generation
	if err := r.Status().Update(ctx, cp); err != nil {
		return fmt.Errorf("failed to claim checkpoint: %w", err)
	}
	log.Info("claimed checkpoint", "checkpoint", klogName(cp), "pod", pod.Name, "path", path)
	return nil
}

// executeCheckpoint dumps the target Pod through the CRI and records the
// outcome. It is idempotent: the destination directory is reset before every
// attempt, because CheckpointPod requires an existing empty directory and a
// retry after a partial failure must not inherit leftovers.
func (r *Reconciler) executeCheckpoint(ctx context.Context, cp *agentsv1alpha1.Checkpoint) error {
	log := logf.FromContext(ctx)
	// Take the finalizer before the first write to disk: artifacts that
	// outlive the Checkpoint CR have no owner left to reclaim them.
	//
	// Adopt the patched object: the patch bumped the resource version, and
	// the terminal status update below would otherwise conflict and requeue a
	// Checkpoint whose dump has already run, costing a second dump.
	patched, err := utils.PatchFinalizer(ctx, r.Client, cp, utils.AddFinalizerOpType, agentsv1alpha1.CheckpointFinalizer)
	if err != nil {
		return err
	}
	cp = patched.(*agentsv1alpha1.Checkpoint)

	pod, err := r.resolvePod(ctx, cp)
	if err != nil {
		return err
	}
	if pod == nil {
		return r.markFailed(ctx, cp, fmt.Errorf("target pod is gone from node %s before the checkpoint started", r.opts.NodeName))
	}

	path, err := r.checkpointPath(cp.Status.CheckpointId)
	if err != nil {
		return r.markFailed(ctx, cp, err)
	}
	if err := resetDir(path); err != nil {
		return r.markFailed(ctx, cp, err)
	}

	sandboxID, err := resolvePodSandboxID(ctx, r.runtime, string(pod.UID))
	if err != nil {
		return r.markFailed(ctx, cp, err)
	}
	containerIDs, err := resolveContainerIDs(ctx, r.runtime, sandboxID, cp.Spec.Containers)
	if err != nil {
		return r.markFailed(ctx, cp, err)
	}

	log.Info("checkpointing pod", "checkpoint", klogName(cp), "pod", pod.Name,
		"podSandboxID", sandboxID, "containers", len(containerIDs), "path", path)
	rpcCtx, cancel := context.WithTimeout(ctx, r.opts.CheckpointTimeout)
	defer cancel()
	_, err = r.runtime.CheckpointPod(rpcCtx, &runtimeapi.CheckpointPodRequest{
		PodSandboxId: sandboxID,
		OutputPath:   path,
		ContainerIds: containerIDs,
	})
	if err != nil {
		// The runtime removes its own partial artifacts, but the directory
		// itself is owned by this agent, so a failed attempt reclaims it
		// rather than leaving an empty directory behind forever.
		if rmErr := os.RemoveAll(path); rmErr != nil {
			log.Error(rmErr, "failed to remove checkpoint directory after a failed dump", "path", path)
		}
		return r.markFailed(ctx, cp, fmt.Errorf("CheckpointPod failed: %w", err))
	}

	return r.markSucceeded(ctx, cp, pod)
}

// reconcileDeleting removes the artifacts this node owns and releases the
// finalizer. A Checkpoint whose location points at another node is left
// alone — that node's agent holds the same finalizer and owns the cleanup.
func (r *Reconciler) reconcileDeleting(ctx context.Context, cp *agentsv1alpha1.Checkpoint) error {
	if !controllerutil.ContainsFinalizer(cp, agentsv1alpha1.CheckpointFinalizer) {
		return nil
	}
	// A finalizer without a location can only come from this agent crashing
	// between taking the finalizer and its first write, so there is nothing
	// on disk and the finalizer is still ours to release.
	if cp.Status.CheckpointLocation != nil && !r.ownsCheckpoint(cp) {
		return nil
	}
	if cp.Status.CheckpointId != "" {
		path, err := r.checkpointPath(cp.Status.CheckpointId)
		if err != nil {
			// An unusable path cannot be reclaimed by retrying, and holding
			// the finalizer would strand the Checkpoint object. Surface it and
			// release.
			logf.FromContext(ctx).Error(err, "skipping checkpoint artifact cleanup", "checkpoint", klogName(cp))
		} else if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("failed to remove checkpoint directory %q: %w", path, err)
		}
	}
	if _, err := utils.PatchFinalizer(ctx, r.Client, cp, utils.RemoveFinalizerOpType, agentsv1alpha1.CheckpointFinalizer); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("released checkpoint artifacts", "checkpoint", klogName(cp))
	return nil
}

// markSucceeded records the pod template delta and the terminal phase. Every
// checkpoint records the delta, not only the pod-info-only ones: a dump
// checkpoint is what the resume path reads to rebuild the pod spec, and the
// Sandbox controller never pairs a dump with a separate pod-info checkpoint.
//
// The delta is best-effort: a successful dump must not be reported as failed
// just because the Sandbox it belongs to is already gone, which only costs the
// resumed Pod its recorded spec drift.
func (r *Reconciler) markSucceeded(ctx context.Context, cp *agentsv1alpha1.Checkpoint, pod *corev1.Pod) error {
	box, err := r.resolveSandbox(ctx, cp)
	if err != nil {
		return err
	}
	if box == nil {
		logf.FromContext(ctx).Info("sandbox not found, recording checkpoint without pod template delta",
			"checkpoint", klogName(cp))
	} else if delta, err := checkpointutils.BuildPodTemplateDelta(pod, box); err != nil {
		logf.FromContext(ctx).Error(err, "failed to build pod template delta", "checkpoint", klogName(cp))
	} else {
		cp.Status.PodTemplateDelta = delta
	}
	cp.Status.Phase = agentsv1alpha1.CheckpointSucceeded
	cp.Status.Message = ""
	cp.Status.CompletionTime = ptrTime(metav1.Now())
	cp.Status.ObservedGeneration = cp.Generation
	if err := r.Status().Update(ctx, cp); err != nil {
		return fmt.Errorf("failed to record checkpoint success: %w", err)
	}
	logf.FromContext(ctx).Info("checkpoint succeeded", "checkpoint", klogName(cp))
	return nil
}

// markFailed records a terminal failure. The cause is returned to the caller
// only through the status: a Checkpoint that cannot be produced is not a
// transient reconcile error, and retrying it would re-run a dump the Sandbox
// controller has already been told to stop waiting for.
func (r *Reconciler) markFailed(ctx context.Context, cp *agentsv1alpha1.Checkpoint, cause error) error {
	logf.FromContext(ctx).Error(cause, "checkpoint failed", "checkpoint", klogName(cp))
	cp.Status.Phase = agentsv1alpha1.CheckpointFailed
	cp.Status.Message = utils.TruncateConditionMessage(cause.Error())
	cp.Status.CompletionTime = ptrTime(metav1.Now())
	cp.Status.ObservedGeneration = cp.Generation
	if err := r.Status().Update(ctx, cp); err != nil {
		return fmt.Errorf("failed to record checkpoint failure %q: %w", cause, err)
	}
	return nil
}

// ownsCheckpoint reports whether the Checkpoint's artifacts belong to this
// node.
func (r *Reconciler) ownsCheckpoint(cp *agentsv1alpha1.Checkpoint) bool {
	loc := cp.Status.CheckpointLocation
	return loc != nil && loc.Type == agentsv1alpha1.CheckpointSourceTypeLocal &&
		loc.Local != nil && loc.Local.NodeName == r.opts.NodeName
}

// resolvePod returns the target Pod of the Checkpoint, or nil when it is not
// on this node. The Pod cache is restricted to this node, so a missing Pod and
// a Pod on another node are indistinguishable here, and both mean the same
// thing: this agent is not the one to execute the Checkpoint.
func (r *Reconciler) resolvePod(ctx context.Context, cp *agentsv1alpha1.Checkpoint) (*corev1.Pod, error) {
	name := ""
	switch {
	case cp.Spec.PodName != nil && *cp.Spec.PodName != "":
		name = *cp.Spec.PodName
	case cp.Spec.SandboxName != nil && *cp.Spec.SandboxName != "":
		// The Sandbox controller names the Pod after the Sandbox.
		name = *cp.Spec.SandboxName
	default:
		return nil, nil
	}
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: name}, pod)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s/%s: %w", cp.Namespace, name, err)
	}
	return pod, nil
}

// resolveSandbox returns the Sandbox the Checkpoint belongs to, or nil when it
// is already gone.
func (r *Reconciler) resolveSandbox(ctx context.Context, cp *agentsv1alpha1.Checkpoint) (*agentsv1alpha1.Sandbox, error) {
	if cp.Spec.SandboxName == nil || *cp.Spec.SandboxName == "" {
		return nil, nil
	}
	box := &agentsv1alpha1.Sandbox{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: *cp.Spec.SandboxName}, box)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get sandbox %s/%s: %w", cp.Namespace, *cp.Spec.SandboxName, err)
	}
	return box, nil
}

// runtimeClassEligible reports whether the Pod's runtime class is one the
// agent was told can be checkpointed.
func (r *Reconciler) runtimeClassEligible(pod *corev1.Pod) bool {
	if len(r.opts.RuntimeClasses) == 0 {
		return true
	}
	if pod.Spec.RuntimeClassName == nil {
		return false
	}
	return slices.Contains(r.opts.RuntimeClasses, *pod.Spec.RuntimeClassName)
}

// checkpointPath resolves the directory of a checkpoint ID under the agent's
// root. The ID ends up in a filesystem path, so it is validated as a single
// path element: the status is writable by anyone who can update a Checkpoint,
// and a crafted ID must not be able to escape the root.
func (r *Reconciler) checkpointPath(checkpointID string) (string, error) {
	if checkpointID == "" || checkpointID != filepath.Base(checkpointID) ||
		checkpointID == "." || checkpointID == ".." {
		return "", fmt.Errorf("invalid checkpoint id %q", checkpointID)
	}
	return filepath.Join(r.opts.CheckpointRoot, checkpointID), nil
}

// checkpointIDFor derives the checkpoint ID from the Checkpoint UID, which is
// unique for the lifetime of the cluster and is a safe single path element.
func checkpointIDFor(cp *agentsv1alpha1.Checkpoint) string {
	return "cp-" + string(cp.UID)
}

// resetDir makes path an existing, empty, caller-only directory, which is what
// CheckpointPod requires of its output path.
func resetDir(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("failed to clear checkpoint directory %q: %w", path, err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("failed to create checkpoint directory %q: %w", path, err)
	}
	return nil
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

func klogName(cp *agentsv1alpha1.Checkpoint) string {
	return cp.Namespace + "/" + cp.Name
}

// closeOnStop releases the runtime connection when the manager stops.
type closeOnStop struct {
	closer interface{ Close() error }
}

func (c closeOnStop) Start(ctx context.Context) error {
	<-ctx.Done()
	return c.closer.Close()
}
