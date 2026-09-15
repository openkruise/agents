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
	"strconv"

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
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	runtimeclient "github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/sidecarutils"
)

// PodGenerateArgs holds the arguments for PodGenerateFunc.
type PodGenerateArgs struct {
	Client    client.Client
	Box       *agentsv1alpha1.Sandbox
	NewStatus *agentsv1alpha1.SandboxStatus
	// IsResume indicates that this pod creation is a resume from a paused
	// state (deep hibernation). The generator uses this to inject resume-only
	// annotations (recover-from-instance-id, source-pod-uid, recreating)
	// from the sandbox status PodInfo, instead of relying on the sandbox
	// phase which may be Upgrading during an upgrade's Resuming stage.
	IsResume bool
	// ProbeManager renders the pod probe annotation for the auto-pause
	// probes declared on the sandbox.
	ProbeManager *PodProbeManager
}

// PodGenerateFunc generates a Pod from a Sandbox spec.
type PodGenerateFunc func(ctx context.Context, args PodGenerateArgs) (*corev1.Pod, error)

// CreatePodArgs holds the arguments for CreatePod.
type CreatePodArgs struct {
	Box              *agentsv1alpha1.Sandbox
	NewStatus        *agentsv1alpha1.SandboxStatus
	PodTemplateDelta *runtime.RawExtension
	CheckpointID     string
	// AdvertiseRuntimeTLS opts this pod creation into the runtime HTTPS
	// capability stamp (see stampRuntimeTLSAnnotation). Call sites that create
	// a pod from the current template (first creation, recreate upgrade) set
	// it. The resume-from-pause path leaves it false: a resumed sandbox was
	// already stamped at first creation (the stamp is write-once), and the
	// checkpoint delta applied on top of the template may override the sidecar
	// configuration, so the current template is not authoritative there.
	AdvertiseRuntimeTLS bool
	// IsResume indicates that this pod creation is a resume from a paused
	// state (deep hibernation). The generator uses this to inject resume-only
	// annotations (recover-from-instance-id, source-pod-uid, recreating)
	// from the sandbox status PodInfo, instead of relying on the sandbox
	// phase which may be Upgrading during an upgrade's Resuming stage.
	// Resume paths that recreate a brand-new pod with no underlying instance
	// to recover (Stop, Snapshot) leave it false: the resume-protocol
	// annotations would make VK skip the pod or try to recover a deleted
	// instance, so those paths use the normal creation flow.
	IsResume bool
}

// PodControl manages Pod creation for sandbox controllers.
type PodControl struct {
	client.Client
	recorder                  record.EventRecorder
	generatePod               PodGenerateFunc
	probeManager              *PodProbeManager
	checkpointIDAnnotationKey string
	// advertiseRuntimeTLS is the cluster-level switch for the runtime HTTPS
	// capability stamp (see SetAdvertiseRuntimeTLS).
	advertiseRuntimeTLS bool
}

// NewPodControl creates a new PodControl.
func NewPodControl(cli client.Client, recorder record.EventRecorder, genFn PodGenerateFunc) *PodControl {
	return &PodControl{
		Client:       cli,
		recorder:     recorder,
		generatePod:  genFn,
		probeManager: NewPodProbeManager(cli, recorder),
	}
}

// SetCheckpointIDAnnotationKey overrides the annotation key used to store the
// checkpoint ID on the Pod. This is configured via the controller flag
// --checkpoint-id-annotation-key. If not set, no checkpoint ID annotation
// is written to pods.
func (c *PodControl) SetCheckpointIDAnnotationKey(key string) {
	if key != "" {
		c.checkpointIDAnnotationKey = key
	}
}

// SetAdvertiseRuntimeTLS enables the runtime HTTPS capability stamp
// (AnnotationRuntimeTLSPort, see stampRuntimeTLSAnnotation) for the call sites
// that opt in via CreatePodArgs.AdvertiseRuntimeTLS. It is derived from the
// controller's own runtime client TLS material (--runtime-client-cert-dir):
// advertising a capability the controller itself cannot consume would only
// create sandboxes nobody can serve, so the client material is the single
// switch for both directions.
//
// Because the pod-side HTTPS server is configured out-of-band (the
// agent-runtime sidecar -enable-tls arguments and certificate mounts in the
// injection ConfigMap), enabling it remains an operator assertion: the
// injection ConfigMap must serve HTTPS *before* the controller is given its
// client certificates. The reverse order would stamp sandboxes whose pod
// listens on no HTTPS port, and the stamp is write-once with no self-healing.
func (c *PodControl) SetAdvertiseRuntimeTLS(enabled bool) {
	c.advertiseRuntimeTLS = enabled
}

// CreatePod generates and creates a Pod for the given sandbox.
func (c *PodControl) CreatePod(ctx context.Context, args CreatePodArgs) (*corev1.Pod, error) {
	box := args.Box

	if shouldInjectCABundles() {
		if err := identity.EnsureAllCACerts(ctx, c.Client, box, box.Namespace); err != nil {
			klog.FromContext(ctx).Error(err, "failed to ensure CA bundle secrets", "sandbox", klog.KObj(box))
			return nil, err
		}
	}

	pod, err := c.generatePod(ctx, PodGenerateArgs{Client: c.Client, Box: box, NewStatus: args.NewStatus, IsResume: args.IsResume, ProbeManager: c.probeManager})
	if err != nil {
		if args.NewStatus != nil {
			utils.SetSandboxCondition(args.NewStatus, metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             agentsv1alpha1.SandboxReadyReasonPodCreateFailed,
				Message:            utils.TruncateConditionMessage(err.Error()),
			})
		}
		return nil, err
	}

	// Set checkpoint ID annotation for CheckpointRestore upgrade.
	// The checkpoint controller uses this to restore the pod's writable layer.
	// Only set the annotation when a custom key is configured via the controller flag.
	if args.CheckpointID != "" && c.checkpointIDAnnotationKey != "" {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[c.checkpointIDAnnotationKey] = args.CheckpointID
	}

	// Apply checkpoint pod template delta if present (resume path).
	// The delta is best-effort: a malformed or otherwise unappliable delta must
	// not block pod creation. Surface the failure via log + Warning event and
	// continue with the freshly generated pod spec.
	if args.PodTemplateDelta != nil {
		klog.V(5).InfoS("Pod spec before checkpoint delta", "sandbox", klog.KObj(box), "pod", utils.DumpJson(pod), "delta", string(args.PodTemplateDelta.Raw))
		if applyErr := checkpointutils.ApplyPodTemplateDelta(pod, *args.PodTemplateDelta); applyErr != nil {
			klog.FromContext(ctx).Error(applyErr, "failed to apply pod template delta from checkpoint, continuing without delta", "sandbox", klog.KObj(box))
			c.recorder.Event(box, corev1.EventTypeWarning, "CheckpointApplyFailed",
				fmt.Sprintf("Failed to apply checkpoint delta, continuing without it: %v", applyErr))
		} else {
			klog.V(5).InfoS("Pod spec after checkpoint delta", "sandbox", klog.KObj(box), "pod", utils.DumpJson(pod))
		}
	}

	// Stamp the runtime HTTPS capability onto the sandbox before the pod is
	// created: if the stamp fails the pod is not created and the whole creation
	// is retried, so a live pod always implies a sandbox that already
	// advertises its capabilities.
	if args.AdvertiseRuntimeTLS && c.advertiseRuntimeTLS {
		if err := c.stampRuntimeTLSAnnotation(ctx, box); err != nil {
			return nil, err
		}
	}

	ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Create, box.Name)
	// Trace the pod creation as a child span. No pod name attribute: the pod
	// name always equals the sandbox name, which the Reconcile span carries.
	ctx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerCreatePod)
	err = c.Create(ctx, pod)
	// AlreadyExists is an idempotent success here (the pod is already in the
	// desired state), so normalize it at this call site; EndSpan itself is
	// policy-neutral because AlreadyExists is a genuine failure elsewhere.
	spanErr := err
	if errors.IsAlreadyExists(spanErr) {
		spanErr = nil
	}
	tracing.EndSpan(ctx, span, spanErr)
	if err != nil {
		ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Create, box.Name)
		if !errors.IsAlreadyExists(err) {
			klog.FromContext(ctx).Error(err, "create pod failed", "sandbox", klog.KObj(box))
			// Emit Warning Event and set Ready condition to reflect the failure
			// so that users can diagnose the root cause (e.g., invalid PVC, quota
			// exceeded, etc.) without digging through controller logs.
			c.recorder.Event(box, corev1.EventTypeWarning, agentsv1alpha1.SandboxReadyReasonPodCreateFailed,
				fmt.Sprintf("Failed to create pod: %v", err))
			utils.SetSandboxCondition(args.NewStatus, metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             agentsv1alpha1.SandboxReadyReasonPodCreateFailed,
				Message:            utils.TruncateConditionMessage(err.Error()),
			})
			return nil, err
		}
	}
	kvs := []any{"sandbox", klog.KObj(box), "pod", klog.KObj(pod)}
	if klog.V(5).Enabled() {
		kvs = append(kvs, "body", utils.DumpJson(pod))
	}
	klog.FromContext(ctx).Info("Create pod success", kvs...)
	return pod, nil
}

// stampRuntimeTLSAnnotation advertises the runtime HTTPS capability by
// persisting AnnotationRuntimeTLSPort on the sandbox with a meta patch.
// Because it runs before the pod Create call, the annotation is durable
// strictly earlier than any pod-derived status transition: an observer that
// sees the sandbox Ready is guaranteed to also see the capability annotation,
// without any pod->sandbox resync. The stamp is write-once and add-only: an
// already present annotation is never updated and never removed, so turning
// the switch off does not resync existing sandboxes — an already stamped
// sandbox keeps advertising HTTPS and the only remedies are a forward fix or
// clearing the annotation.
func (c *PodControl) stampRuntimeTLSAnnotation(ctx context.Context, box *agentsv1alpha1.Sandbox) error {
	// Only advertise the capability for sandboxes that actually get the
	// agent-runtime sidecar injected: the stamp is add-only, so advertising
	// HTTPS for a pod without the runtime sidecar would send TLS-capable
	// clients to a port nobody listens on, with no self-healing path.
	if !sidecarutils.IsRuntimeEnabled(box, agentsv1alpha1.RuntimeConfigForInjectAgentRuntime) {
		return nil
	}
	if _, ok := box.Annotations[agentsv1alpha1.AnnotationRuntimeTLSPort]; ok {
		// Write-once: never overwrite an already stamped capability.
		return nil
	}
	// Stamp box directly against a pre-mutation snapshot: a successful patch
	// leaves the in-memory sandbox already in sync for the rest of the
	// reconcile, and on failure the returned error aborts pod creation, so the
	// locally mutated object is discarded with the reconcile.
	patch := client.MergeFrom(box.DeepCopy())
	if box.Annotations == nil {
		box.Annotations = map[string]string{}
	}
	box.Annotations[agentsv1alpha1.AnnotationRuntimeTLSPort] = strconv.Itoa(runtimeclient.RuntimeTLSPort)
	if err := c.Patch(ctx, box, patch); err != nil {
		klog.ErrorS(err, "failed to stamp runtime TLS annotation on sandbox", "sandbox", klog.KObj(box))
		c.recorder.Event(box, corev1.EventTypeWarning, "RuntimeTLSStampFailed",
			fmt.Sprintf("Failed to stamp runtime TLS annotation: %v", err))
		return fmt.Errorf("failed to stamp runtime TLS annotation: %w", err)
	}
	klog.InfoS("stamped runtime TLS annotation on sandbox", "sandbox", klog.KObj(box))
	return nil
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
// It is the default PodGenerateFunc for the common control path and is responsible
// for generating the full pod (template + PVC volumes + sidecar/runtime injection).
func GeneratePodFromSandbox(ctx context.Context, args PodGenerateArgs) (*corev1.Pod, error) {
	pod, err := generateBasePodFromSandbox(ctx, args)
	if err != nil {
		return nil, err
	}
	// Inject sandbox runtime/CSI sidecars (community variant). Generators owned by
	// other control modes are responsible for invoking their own
	// injection variant (e.g. InjectSandboxRuntimesUsingCache) so that PodControl
	// stays generator-agnostic and does not double-inject.
	if err := sidecarutils.InjectSandboxRuntimes(ctx, args.Box, pod, args.Client); err != nil {
		klog.FromContext(ctx).Error(err, "failed to inject pod template with csi sidecar or runtime sidecar", "sandbox", klog.KObj(args.Box))
		return nil, err
	}
	return pod, nil
}

// generateBasePodFromSandbox builds the pod template + PVC volumes from the sandbox
// spec without performing any sidecar/runtime injection. It is the shared building
// block for both the community generator (GeneratePodFromSandbox) and others, each of which decides which sidecar
// injection variant to apply afterwards.
func generateBasePodFromSandbox(ctx context.Context, args PodGenerateArgs) (*corev1.Pod, error) {
	cli, box := args.Client, args.Box
	var revision string
	if args.NewStatus != nil {
		revision = args.NewStatus.UpdateRevision
	}
	podTemplate, err := utils.GetTemplateSpec(ctx, cli, box.Namespace, &box.Spec.EmbeddedSandboxTemplate)
	if err != nil {
		if box.Spec.TemplateRef != nil {
			klog.FromContext(ctx).Error(err, "failed to get sandbox template", "sandbox", klog.KObj(box), "template", box.Spec.TemplateRef.Name)
		} else {
			klog.FromContext(ctx).Error(err, "failed to get sandbox template", "sandbox", klog.KObj(box))
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
			klog.FromContext(ctx).Error(err, "failed to generate PVC name", "sandbox", klog.KObj(box), "template", template.Name)
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

	// Inject lifecycle probes as kruise.io/podprobe annotation (PodProbeMarker
	// Serverless protocol). The agent-runtime sidecar reads this annotation,
	// executes probes periodically, and writes results to Pod.Status.Conditions.
	if args.ProbeManager != nil {
		args.ProbeManager.InjectProbe(ctx, box, pod)
	}

	return pod, nil
}
