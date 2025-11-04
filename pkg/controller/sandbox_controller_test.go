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

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	agentsv1alpha1 "gitlab.alibaba-inc.com/serverlessinfra/agents/api/v1alpha1"
	"gitlab.alibaba-inc.com/serverlessinfra/agents/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	scheme  *runtime.Scheme
	nt      = metav1.Now()
	ot      = metav1.NewTime(time.Date(2025, 9, 28, 11, 0, 0, 0, time.Local))
	boxDemo = &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-box-1",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "mirrors-ssl.aliyuncs.com/centos:centos7",
						},
					},
				},
			},
		},
	}

	podDemo = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-box-1",
			Namespace: "default",
			Annotations: map[string]string{
				utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(boxDemo, sandboxControllerKind)},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "mirrors-ssl.aliyuncs.com/centos:centos7",
				},
			},
			NodeName: "virtual-kubelet-cn-beijing-d",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "172.17.0.61",
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: ot,
				},
			},
		},
	}
)

func init() {
	scheme = runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
}

func TestSandboxReconcile(t *testing.T) {
	type Case struct {
		name        string
		getSandbox  func() *agentsv1alpha1.Sandbox
		getPod      func() *corev1.Pod
		expected    func() *agentsv1alpha1.Sandbox
		expectedPod func() *corev1.Pod
	}
	cases := []Case{
		{
			name: "sandbox pending",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				return obj
			},
			getPod: func() *corev1.Pod {
				return nil
			},
			expected: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				}
				return obj
			},
			expectedPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Status = corev1.PodStatus{}
				obj.Annotations = map[string]string{}
				obj.Spec.NodeName = ""
				return obj
			},
		},
		{
			name: "sandbox running, pod not ready",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				}
				return obj
			},
			getPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Status.Conditions = []corev1.PodCondition{
					{
						Type:               corev1.PodReady,
						Status:             corev1.ConditionFalse,
						LastTransitionTime: ot,
					},
				}
				return obj
			},
			expected: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			expectedPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Status.Conditions = []corev1.PodCondition{
					{
						Type:               corev1.PodReady,
						Status:             corev1.ConditionFalse,
						LastTransitionTime: ot,
					},
				}
				return obj
			},
		},
		{
			name: "sandbox running, pod ready",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			getPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Status.Conditions = []corev1.PodCondition{
					{
						Type:               corev1.PodReady,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: ot,
					},
				}
				return obj
			},
			expected: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: ot,
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			expectedPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Status.Conditions = []corev1.PodCondition{
					{
						Type:               corev1.PodReady,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: ot,
					},
				}
				return obj
			},
		},
		{
			name: "sandbox paused, SetPause",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Spec.Paused = true
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: ot,
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			getPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Status.Conditions = []corev1.PodCondition{
					{
						Type:               corev1.PodReady,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: ot,
					},
				}
				return obj
			},
			expected: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = true
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxPausedReasonSetPause,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			expectedPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Annotations = map[string]string{
					utils.PodAnnotationAcsInstanceId:   "acs-2ze00987m29zidm3kiwy",
					utils.PodAnnotationPause:           "true",
					utils.PodAnnotationReserveInstance: "true",
				}
				obj.Status.Conditions = []corev1.PodCondition{
					{
						Type:               corev1.PodReady,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: ot,
					},
				}
				return obj
			},
		},
		{
			name: "sandbox paused, DeletePod",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = true
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxPausedReasonDeletePod,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			getPod: func() *corev1.Pod {
				return nil
			},
			expected: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = true
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionTrue,
							Reason:             agentsv1alpha1.SandboxPausedReasonDeletePod,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			expectedPod: func() *corev1.Pod {
				return nil
			},
		},
		{
			name: "sandbox resume, CreatePod",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = false
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionTrue,
							Reason:             agentsv1alpha1.SandboxPausedReasonDeletePod,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			getPod: func() *corev1.Pod {
				return nil
			},
			expected: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = false
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			expectedPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Annotations = map[string]string{
					utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
					utils.PodAnnotationPause:         "true",
				}
				obj.Status = corev1.PodStatus{}
				return obj
			},
		},
		{
			name: "sandbox resume, ResumePod",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = false
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxResumeReasonResumePod,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			getPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Annotations = map[string]string{
					utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
					utils.PodAnnotationPause:         "true",
				}
				obj.Status = corev1.PodStatus{}
				return obj
			},
			expected: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = false
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxResumeReasonResumePod,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			expectedPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Annotations = map[string]string{
					utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
					utils.PodAnnotationPause:         "false",
				}
				obj.Status = corev1.PodStatus{}
				return obj
			},
		},
		{
			name: "sandbox running, again",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = false
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionResumed),
							Status:             metav1.ConditionFalse,
							Reason:             agentsv1alpha1.SandboxResumeReasonResumePod,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			getPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Annotations = map[string]string{
					utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
					utils.PodAnnotationPause:         "false",
				}
				return obj
			},
			expected: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = false
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: ot,
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			expectedPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Annotations = map[string]string{
					utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
					utils.PodAnnotationPause:         "false",
				}
				return obj
			},
		},
		{
			name: "delete sandbox when paused",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = true
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionTrue,
							Reason:             agentsv1alpha1.SandboxPausedReasonDeletePod,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			getPod: func() *corev1.Pod {
				return nil
			},
			expected: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Annotations = map[string]string{
					utils.SandboxAnnotationEnableVKDeleteInstance: "true",
				}
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = true
				obj.DeletionTimestamp = &nt
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxTerminating,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionTrue,
							Reason:             agentsv1alpha1.SandboxPausedReasonDeletePod,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			expectedPod: func() *corev1.Pod {
				return nil
			},
		},
		{
			name: "delete sandbox when running",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = false
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			getPod: func() *corev1.Pod {
				obj := podDemo.DeepCopy()
				obj.Annotations = map[string]string{
					utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
					utils.PodAnnotationPause:         "false",
				}
				return obj
			},
			expected: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = false
				obj.DeletionTimestamp = &nt
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxTerminating,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			expectedPod: func() *corev1.Pod {
				return nil
			},
		},
		{
			name: "delete sandbox when running, 2",
			getSandbox: func() *agentsv1alpha1.Sandbox {
				obj := boxDemo.DeepCopy()
				obj.Finalizers = []string{utils.SandboxFinalizer}
				obj.Spec.Paused = false
				obj.DeletionTimestamp = &nt
				obj.Status = agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxTerminating,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						Annotations: map[string]string{
							utils.PodAnnotationAcsInstanceId: "acs-2ze00987m29zidm3kiwy",
						},
						NodeName: "virtual-kubelet-cn-beijing-d",
						PodIP:    "172.17.0.61",
					},
				}
				return obj
			},
			getPod: func() *corev1.Pod {
				return nil
			},
			expected: func() *agentsv1alpha1.Sandbox {
				return nil
			},
			expectedPod: func() *corev1.Pod {
				return nil
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			box := cs.getSandbox()
			_ = fakeClient.Create(context.TODO(), box)
			pod := cs.getPod()
			if pod != nil {
				_ = fakeClient.Create(context.TODO(), pod)
			}
			if strings.Contains(cs.name, "delete sandbox") {
				_ = fakeClient.Delete(context.TODO(), box)
			}
			reconciler := SandboxReconciler{
				Client: fakeClient,
				Scheme: scheme,
			}
			if _, err := reconciler.Reconcile(context.TODO(), ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: boxDemo.Namespace,
				Name:      boxDemo.Name,
			}}); err != nil {
				t.Errorf("reconcile failed, err: %v", err)
			}

			expectedBox := cs.expected()
			newBox := &agentsv1alpha1.Sandbox{}
			err := fakeClient.Get(context.TODO(), types.NamespacedName{Name: boxDemo.Name, Namespace: boxDemo.Namespace}, newBox)
			if err != nil {
				if errors.IsNotFound(err) {
					newBox = nil
				} else {
					t.Errorf("get Sandbox failed, err: %v", err)
				}
			} else {
				newBox.ResourceVersion = ""
			}
			expectedStr := utils.DumpJson(expectedBox)
			newStr := utils.DumpJson(newBox)
			if expectedStr != newStr {
				t.Fatalf("expect(%s), but get(%s)", expectedStr, newStr)
			}

			expectedPod := cs.expectedPod()
			newPod := &corev1.Pod{}
			err = fakeClient.Get(context.TODO(), types.NamespacedName{Name: podDemo.Name, Namespace: podDemo.Namespace}, newPod)
			if err != nil {
				if errors.IsNotFound(err) {
					newPod = nil
				} else {
					t.Errorf("get Pod failed, err: %v", err)
				}
			} else {
				newPod.ResourceVersion = ""
			}
			expectedPodStr := utils.DumpJson(expectedPod)
			newPodStr := utils.DumpJson(newPod)
			if expectedPodStr != newPodStr {
				t.Fatalf("expect(%s), but get(%s)", expectedPodStr, newPodStr)
			}
		})
	}
}
