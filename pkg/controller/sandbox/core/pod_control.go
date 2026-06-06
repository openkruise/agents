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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	checkpointutils "github.com/openkruise/agents/pkg/controller/checkpoint"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	"github.com/openkruise/agents/pkg/utils/sidecarutils"
)

// PodGenerateArgs holds the arguments for PodGenerateFunc.
type PodGenerateArgs struct {
	Client   client.Client
	Box      *agentsv1alpha1.Sandbox
	Revision string
}

// PodGenerateFunc generates a Pod from a Sandbox spec.
type PodGenerateFunc func(ctx context.Context, args PodGenerateArgs) (*corev1.Pod, error)

// CreatePodArgs holds the arguments for CreatePod.
type CreatePodArgs struct {
	Box              *agentsv1alpha1.Sandbox
	NewStatus        *agentsv1alpha1.SandboxStatus
	PodTemplateDelta *runtime.RawExtension
}

// PodControl manages Pod creation for sandbox controllers.
type PodControl struct {
	client.Client
	recorder    record.EventRecorder
	generatePod PodGenerateFunc
}

// NewPodControl creates a new PodControl.
func NewPodControl(cli client.Client, recorder record.EventRecorder, genFn PodGenerateFunc) *PodControl {
	return &PodControl{Client: cli, recorder: recorder, generatePod: genFn}
}

// CreatePod generates and creates a Pod for the given sandbox.
func (c *PodControl) CreatePod(ctx context.Context, args CreatePodArgs) (*corev1.Pod, error) {
	box := args.Box

	if shouldInjectCABundles() {
		if err := identity.EnsureAllCACerts(ctx, c.Client, box, box.Namespace); err != nil {
			klog.ErrorS(err, "failed to ensure CA bundle secrets", "sandbox", klog.KObj(box))
			return nil, err
		}
	}

	pod, err := c.generatePod(ctx, PodGenerateArgs{Client: c.Client, Box: box, Revision: args.NewStatus.UpdateRevision})
	if err != nil {
		return nil, err
	}

	// to avoid the performance issue, using the controller to inject csi containers
	// fetch the configmap and parse the configuration based on the controller runtime
	injectErr := sidecarutils.InjectSandboxRuntimes(ctx, box, pod, c.Client)
	if injectErr != nil {
		klog.ErrorS(injectErr, "failed to inject pod template with csi sidecar or runtime sidecar", "sandbox", klog.KObj(box))
		return nil, injectErr
	}

	// Apply checkpoint pod template delta if present (resume path).
	// The delta is best-effort: a malformed or otherwise unappliable delta must
	// not block pod creation. Surface the failure via log + Warning event and
	// continue with the freshly generated pod spec.
	if args.PodTemplateDelta != nil {
		klog.V(5).InfoS("Pod spec before checkpoint delta", "sandbox", klog.KObj(box), "pod", utils.DumpJson(pod), "delta", string(args.PodTemplateDelta.Raw))
		if applyErr := checkpointutils.ApplyPodTemplateDelta(pod, *args.PodTemplateDelta); applyErr != nil {
			klog.ErrorS(applyErr, "failed to apply pod template delta from checkpoint, continuing without delta", "sandbox", klog.KObj(box))
			c.recorder.Event(box, corev1.EventTypeWarning, "CheckpointApplyFailed",
				fmt.Sprintf("Failed to apply checkpoint delta, continuing without it: %v", applyErr))
		} else {
			klog.V(5).InfoS("Pod spec after checkpoint delta", "sandbox", klog.KObj(box), "pod", utils.DumpJson(pod))
		}
	}

	ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Create, box.Name)
	err = c.Create(ctx, pod)
	if err != nil {
		ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Create, box.Name)
		if !errors.IsAlreadyExists(err) {
			klog.ErrorS(err, "create pod failed", "sandbox", klog.KObj(box))
			return nil, err
		}
	}
	kvs := []any{"sandbox", klog.KObj(box), "pod", klog.KObj(pod)}
	if klog.V(5).Enabled() {
		kvs = append(kvs, "body", utils.DumpJson(pod))
	}
	klog.InfoS("Create pod success", kvs...)
	return pod, nil
}

// shouldInjectCABundles is the cluster-level kill switch for the CA bundle
// ensure/inject pipeline. It only checks SecurityIdentityProviderGate; whether
// a particular sandbox actually needs a given CA spec is decided exclusively
// by that spec's EnabledFor predicate (bound via identity.BindCAEnabledFor at
// controller startup). Keeping the runtime-level decision in a single place
// avoids drift between the caller-side gate and the per-spec predicate.
func shouldInjectCABundles() bool {
	return utilfeature.DefaultFeatureGate.Enabled(features.SecurityIdentityProviderGate)
}

// GeneratePodFromSandbox creates a Pod object from a Sandbox spec and its template.
func GeneratePodFromSandbox(ctx context.Context, args PodGenerateArgs) (*corev1.Pod, error) {
	cli, box, revision := args.Client, args.Box, args.Revision
	podTemplate, err := utils.GetTemplateSpec(ctx, cli, box.Namespace, &box.Spec.EmbeddedSandboxTemplate)
	if err != nil {
		if box.Spec.TemplateRef != nil {
			klog.ErrorS(err, "failed to get sandbox template", "sandbox", klog.KObj(box), "template", box.Spec.TemplateRef.Name)
		} else {
			klog.ErrorS(err, "failed to get sandbox template", "sandbox", klog.KObj(box))
		}
		return nil, err
	}
	if podTemplate == nil {
		return nil, fmt.Errorf("pod template not found in sandbox %s/%s", box.Namespace, box.Name)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       box.Namespace,
			Name:            box.Name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, sandboxControllerKind)},
			Labels:          podTemplate.Labels,
			Annotations:     podTemplate.Annotations,
		},
		Spec: podTemplate.Spec,
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[utils.PodAnnotationCreatedBy] = utils.CreatedBySandbox
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[utils.PodLabelCreatedBy] = utils.CreatedBySandbox
	// todo, when resume, create Pod based on the revision from the paused state.
	pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = revision

	volumes := make([]corev1.Volume, 0, len(box.Spec.VolumeClaimTemplates))
	for _, template := range box.Spec.VolumeClaimTemplates {
		pvcName, err := GeneratePVCName(template.Name, box.Name)
		if err != nil {
			klog.ErrorS(err, "failed to generate PVC name", "sandbox", klog.KObj(box), "template", template.Name)
			return nil, err
		}
		volumes = append(volumes, corev1.Volume{
			Name: template.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
					ReadOnly:  false,
				},
			},
		})
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, volumes...)
	return pod, nil
}
