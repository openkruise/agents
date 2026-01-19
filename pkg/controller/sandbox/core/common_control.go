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

package core

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

const CommonControlName = "common"

type commonControl struct {
	client.Client
	recorder             record.EventRecorder
	inplaceUpdateControl *inplaceupdate.InPlaceUpdateControl
}

func NewCommonControl(c client.Client, recorder record.EventRecorder) SandboxControl {
	control := &commonControl{
		Client:               c,
		recorder:             recorder,
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(c, inplaceupdate.DefaultGeneratePatchBodyFunc),
	}
	return control
}

func (r *commonControl) EnsureSandboxRunning(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	// If the Pod does not exist, it must first be created.
	if pod == nil {
		if _, err := r.createPod(ctx, box, newStatus); err != nil {
			return err
		}
		return nil
	}

	// pod status running
	if pod.Status.Phase == corev1.PodRunning {
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
		if cond == nil {
			cond = &metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
			}
		}
		if pCond != nil && string(pCond.Status) != string(cond.Status) {
			cond.Status = metav1.ConditionStatus(pCond.Status)
			cond.LastTransitionTime = pCond.LastTransitionTime
		}
		utils.SetSandboxCondition(newStatus, *cond)
		return nil
	}

	return nil
}

func (r *commonControl) EnsureSandboxUpdated(ctx context.Context, args EnsureFuncArgs) error {
	pod, _, newStatus := args.Pod, args.Box, args.NewStatus
	logger := logf.FromContext(ctx).WithValues("pod", klog.KObj(pod))
	// If a Pod is no longer present in the Running state, it should be considered an abnormal situation.
	if pod == nil {
		newStatus.Phase = agentsv1alpha1.SandboxFailed
		newStatus.Message = "Sandbox Pod Not Found"
		return nil
	}

	// update sandbox status
	newStatus.NodeName = pod.Spec.NodeName
	newStatus.SandboxIp = pod.Status.PodIP
	newStatus.PodInfo = agentsv1alpha1.PodInfo{
		PodIP:    pod.Status.PodIP,
		NodeName: pod.Spec.NodeName,
		PodUID:   pod.UID,
	}
	logger.Info("sandbox newStatus", "newStatus", utils.DumpJson(newStatus))
	// inplace update
	done, err := r.handleInplaceUpdateSandbox(ctx, args)
	if err != nil {
		return err
	} else if !done {
		return nil
	}

	pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
	if pCond != nil && string(pCond.Status) != string(cond.Status) {
		cond.Status = metav1.ConditionStatus(pCond.Status)
		cond.LastTransitionTime = pCond.LastTransitionTime
	}
	for _, cStatus := range pod.Status.ContainerStatuses {
		// indicating container startup failure
		if cond.Status == metav1.ConditionFalse && cStatus.State.Waiting != nil {
			cond.Reason = agentsv1alpha1.SandboxReadyReasonStartContainerFailed
			cond.Message = cStatus.State.Waiting.Message
		}
	}
	utils.SetSandboxCondition(newStatus, *cond)
	return nil
}

func (r *commonControl) EnsureSandboxPaused(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box), "pod", klog.KObj(pod))
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if cond == nil {
		cond = &metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionPaused),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxPausedReasonDeletePod,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, *cond)
	} else if cond.Status == metav1.ConditionTrue {
		return nil
	}

	// The paused phase sets condition ready to false.
	if rCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady)); rCond != nil && rCond.Status == metav1.ConditionTrue {
		rCond.Status = metav1.ConditionFalse
		rCond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *rCond)
	}

	// Pod deletion completed, paused completed
	// cond.Status == metav1.ConditionFalse just for sure
	if pod == nil && cond.Status == metav1.ConditionFalse {
		cond.Status = metav1.ConditionTrue
		cond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *cond)
		return nil
	}
	// Pod deletion incomplete, waiting
	if !pod.DeletionTimestamp.IsZero() {
		logger.Info("Sandbox wait pod paused")
		return nil
	}
	err := client.IgnoreNotFound(r.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: ptr.To(int64(30))}))
	if err != nil {
		logger.Error(err, "Delete pod failed")
		return err
	}
	logger.Info("Delete pod success")
	return nil
}

func (r *commonControl) EnsureSandboxResumed(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus

	// Consider the scenario where a pod is paused and immediately resumed,
	// pod phase may be Running, but the actual state could be Terminating.
	if pod != nil && !pod.DeletionTimestamp.IsZero() {
		return fmt.Errorf("the pods created in the previous stage are still in the terminating state.")
	}

	// first create pod
	var err error
	if pod == nil {
		_, err = r.createPod(ctx, box, newStatus)
		return err
	}

	// create pod success, set resumed condition to true
	if resumedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed)); resumedCond != nil && resumedCond.Status == metav1.ConditionFalse {
		resumedCond.Status = metav1.ConditionTrue
		resumedCond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *resumedCond)
	}

	if pod.Status.Phase == corev1.PodRunning {
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
		rCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
		if pCond != nil && string(pCond.Status) != string(rCond.Status) {
			rCond.Status = metav1.ConditionStatus(pCond.Status)
			rCond.LastTransitionTime = pCond.LastTransitionTime
		}
		utils.SetSandboxCondition(newStatus, *rCond)
	}
	return nil
}

func (r *commonControl) EnsureSandboxTerminated(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, _ := args.Pod, args.Box, args.NewStatus
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	var err error
	if pod == nil {
		_, err = utils.PatchFinalizer(ctx, r.Client, box, utils.RemoveFinalizerOpType, utils.SandboxFinalizer)
		if err != nil {
			logger.Error(err, "update sandbox finalizer failed")
			return err
		}
		logger.Info("remove sandbox finalizer success")
		return nil
	} else if !pod.DeletionTimestamp.IsZero() {
		logger.Info("Pod is in deleting, and wait a moment")
		return nil
	}

	err = client.IgnoreNotFound(r.Delete(ctx, pod))
	if err != nil {
		logger.Error(err, "delete pod failed")
		return err
	}
	logger.Info("delete pod success")
	return nil
}

func (r *commonControl) createPod(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) (*corev1.Pod, error) {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       box.Namespace,
			Name:            box.Name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, sandboxControllerKind)},
			Labels:          box.Spec.Template.Labels,
			Annotations:     box.Spec.Template.Annotations,
		},
		Spec: box.Spec.Template.Spec,
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[utils.PodAnnotationCreatedBy] = utils.CreatedBySandbox
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	// todo, when resume, create Pod based on the revision from the paused state.
	pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = newStatus.UpdateRevision

	volumes := make([]corev1.Volume, 0, len(box.Spec.VolumeClaimTemplates))
	for _, template := range box.Spec.VolumeClaimTemplates {
		pvcName, err := GeneratePVCName(template.Name, box.Name)
		if err != nil {
			logger.Error(err, "failed to generate PVC name", "template", template.Name, "sandbox", box.Name)
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

	err := r.Create(ctx, pod)
	if err != nil && !errors.IsAlreadyExists(err) {
		logger.Error(err, "create pod failed")
		return nil, err
	}
	logger.Info("Create pod success", "Body", utils.DumpJson(pod))
	return pod, nil
}

func (r *commonControl) handleInplaceUpdateSandbox(ctx context.Context, args EnsureFuncArgs) (bool, error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	handler := &CommonInPlaceUpdateHandler{
		control:  r.inplaceUpdateControl,
		recorder: r.recorder,
	}
	return handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
}
