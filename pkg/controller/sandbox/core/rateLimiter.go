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

package core

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func init() {
	flag.IntVar(&prioritySandboxThreshold, "priority-sandbox-threshold", prioritySandboxThreshold, "Maximum number of priority sandboxes allowed in creating state concurrently. When exceeded, normal sandbox creation is delayed. Default: 100.")
	flag.IntVar(&maxSandboxCreateDelay, "max-sandbox-create-delay", maxSandboxCreateDelay, "How long in seconds a low-priority sandbox creation request is delayed due to rate limiting. Default: 60")
}

var (
	prioritySandboxThreshold = 100
	maxSandboxCreateDelay    = 60

	AddSandboxTrackAction    = "add"
	DeleteSandboxTrackAction = "delete"
)

func MaxSandboxCreateDelay() int {
	return maxSandboxCreateDelay
}

func PrioritySandboxThreshold() int {
	return prioritySandboxThreshold
}

type SandboxTrack struct {
	Namespace string
	Name      string
}

type RateLimiter struct {
	mu                       sync.RWMutex
	highPrioritySandboxTrack map[string]*SandboxTrack // key: "namespace/name"
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		highPrioritySandboxTrack: map[string]*SandboxTrack{},
	}
}

// getRateLimitDuration applies rate limiting for normal sandboxes when high-priority sandboxes are being created.
// Returns (requeueAfter, true) if rate limiting is applied and reconciliation should stop.
func (r *RateLimiter) getRateLimitDuration(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox) (time.Duration, bool) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxCreatePodRateLimitGate) {
		return 0, false
	}

	// Process the scenario where sandbox enters for the first time
	if IsHighPrioritySandbox(ctx, box) {
		_ = r.UpdateRateLimiter(box)
		// Only rate-limit normal sandbox Pod creation
		return 0, false
	}

	count := r.getPrioritySandboxTrackCount()
	// Normal sandboxes exceeding maxCreateSandboxDelay are no longer blocked from creation
	if time.Since(box.CreationTimestamp.Time) < (time.Duration(maxSandboxCreateDelay)*time.Second) &&
		count > prioritySandboxThreshold {
		klog.InfoS("high creating sandbox count exceed threshold, and wait", "sandbox", klog.KObj(box),
			"current creating count", count, "prioritySandboxThreshold", prioritySandboxThreshold)
		// TODO: Trigger on-demand instead of periodic requeue
		return time.Second * 3, true
	}

	return 0, false
}

func (r *RateLimiter) UpdateRateLimiter(box *agentsv1alpha1.Sandbox) bool {
	isCreating := isCreatingSandbox(box)
	key := fmt.Sprintf("%s/%s", box.Namespace, box.Name)
	var isCreatingTimeout bool
	if isCreating {
		// To prevent high-priority sandboxes that remain Not Ready from blocking normal sandbox creation,
		// sandboxes exceeding maxCreateSandboxDelay are no longer added to the highPrioritySandboxTrack queue
		isCreatingTimeout = time.Since(box.CreationTimestamp.Time) > (time.Duration(maxSandboxCreateDelay) * time.Second)
	}

	action := AddSandboxTrackAction
	if !isCreating || isCreatingTimeout {
		action = DeleteSandboxTrackAction
	}

	r.mu.RLock()
	_, inCreatingTrack := r.highPrioritySandboxTrack[key]
	r.mu.RUnlock()

	switch action {
	case AddSandboxTrackAction:
		if inCreatingTrack {
			return true
		}
		r.mu.Lock()
		track := SandboxTrack{
			Namespace: box.Namespace,
			Name:      box.Name,
		}
		r.highPrioritySandboxTrack[key] = &track
		inCreatingTrack = true
		r.mu.Unlock()
		// delete
	default:
		if !inCreatingTrack {
			return false
		}
		r.mu.Lock()
		delete(r.highPrioritySandboxTrack, key)
		inCreatingTrack = false
		r.mu.Unlock()
	}
	return inCreatingTrack
}

func (r *RateLimiter) getPrioritySandboxTrackCount() int {
	r.mu.RLock()
	count := len(r.highPrioritySandboxTrack)
	r.mu.RUnlock()
	return count
}

func IsHighPrioritySandbox(ctx context.Context, box *agentsv1alpha1.Sandbox) bool {
	value, ok := box.Annotations[agentsv1alpha1.SandboxAnnotationPriority]
	if !ok || value == "" {
		return false
	}

	priority, err := strconv.Atoi(value)
	if err != nil {
		klog.ErrorS(err, "parse annotations failed", "sandbox", klog.KObj(box), agentsv1alpha1.SandboxAnnotationPriority, value)
		return false
	}
	return priority > 0
}

func isCreatingSandbox(box *agentsv1alpha1.Sandbox) bool {
	if !box.DeletionTimestamp.IsZero() {
		return false
	}
	if box.Status.Phase == agentsv1alpha1.SandboxPaused || box.Status.Phase == agentsv1alpha1.SandboxResuming ||
		box.Status.Phase == agentsv1alpha1.SandboxSucceeded || box.Status.Phase == agentsv1alpha1.SandboxFailed {
		return false
	}
	cond := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.SandboxConditionReady))
	if box.Status.Phase == agentsv1alpha1.SandboxRunning && cond != nil && cond.Status == metav1.ConditionTrue {
		return false
	}
	return true
}
