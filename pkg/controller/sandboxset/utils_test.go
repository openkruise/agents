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

package sandboxset

import (
	"testing"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var scheme *runtime.Scheme

func init() {
	scheme = runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
}

func TestInitNewStatus(t *testing.T) {
	tests := []struct {
		name       string
		sandboxSet *agentsv1alpha1.SandboxSet
	}{
		{
			name: "basic template with single container",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandboxset",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					SandboxTemplate: agentsv1alpha1.SandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "nginx",
										Image: "nginx:1.19",
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("100m"),
												corev1.ResourceMemory: resource.MustParse("128Mi"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "template with multiple containers",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "multi-container-set",
					Namespace:  "default",
					Generation: 2,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					SandboxTemplate: agentsv1alpha1.SandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "nginx",
										Image: "nginx:1.19",
									},
									{
										Name:  "sidecar",
										Image: "busybox:latest",
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "template with init containers",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "init-container-set",
					Namespace:  "default",
					Generation: 3,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					SandboxTemplate: agentsv1alpha1.SandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								InitContainers: []corev1.Container{
									{
										Name:  "init",
										Image: "busybox:1.35",
									},
								},
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "nginx:1.19",
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "template with empty containers",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "empty-container-set",
					Namespace:  "default",
					Generation: 4,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					SandboxTemplate: agentsv1alpha1.SandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{},
							},
						},
					},
				},
			},
		},
		{
			name: "template with volumes and other fields",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "volume-set",
					Namespace:  "default",
					Generation: 5,
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					SandboxTemplate: agentsv1alpha1.SandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app": "test",
								},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:latest",
									},
								},
								Volumes: []corev1.Volume{
									{
										Name: "test-volume",
										VolumeSource: corev1.VolumeSource{
											EmptyDir: &corev1.EmptyDirVolumeSource{},
										},
									},
								},
								NodeSelector: map[string]string{
									"kubernetes.io/os": "linux",
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			reconciler := &Reconciler{
				Client: fakeClient,
				Scheme: scheme,
			}

			status1, err := reconciler.initNewStatus(tt.sandboxSet)
			if err != nil {
				t.Errorf("initNewStatus() error = %v", err)
				return
			}

			if status1.UpdateRevision == "" {
				t.Errorf("initNewStatus() returned empty UpdateRevision")
			}
			if status1.ObservedGeneration != tt.sandboxSet.Generation {
				t.Errorf("initNewStatus() ObservedGeneration = %v, want %v", status1.ObservedGeneration, tt.sandboxSet.Generation)
			}

			status2, err := reconciler.initNewStatus(tt.sandboxSet)
			if err != nil {
				t.Errorf("initNewStatus() second call error = %v", err)
				return
			}
			if status1.UpdateRevision != status2.UpdateRevision {
				t.Errorf("initNewStatus() is not consistent for UpdateRevision: got %s, want %s", status1.UpdateRevision, status2.UpdateRevision)
			}

			if len(status1.UpdateRevision) < 5 {
				t.Errorf("initNewStatus() returned UpdateRevision that is too short: %s", status1.UpdateRevision)
			}
		})
	}
}

func TestInitNewStatusWithDifferentImages(t *testing.T) {
	sandboxSet1 := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-sandboxset-1",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:1.19",
							},
						},
					},
				},
			},
		},
	}

	sandboxSet2 := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-sandboxset-2",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:1.20",
							},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &Reconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	status1, err := reconciler.initNewStatus(sandboxSet1)
	if err != nil {
		t.Errorf("initNewStatus() error = %v", err)
		return
	}

	status2, err := reconciler.initNewStatus(sandboxSet2)
	if err != nil {
		t.Errorf("initNewStatus() error = %v", err)
		return
	}

	if status1.UpdateRevision == status2.UpdateRevision {
		t.Errorf("Expected different UpdateRevision for different images, but got same: %s", status1.UpdateRevision)
	}
}

func TestInitNewStatusWithDifferentResources(t *testing.T) {
	sandboxSet1 := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-sandboxset-1",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("100m"),
										corev1.ResourceMemory: resource.MustParse("128Mi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	sandboxSet2 := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-sandboxset-2",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("200m"),
										corev1.ResourceMemory: resource.MustParse("256Mi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &Reconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	status1, err := reconciler.initNewStatus(sandboxSet1)
	if err != nil {
		t.Errorf("initNewStatus() error = %v", err)
		return
	}

	status2, err := reconciler.initNewStatus(sandboxSet2)
	if err != nil {
		t.Errorf("initNewStatus() error = %v", err)
		return
	}

	if status1.UpdateRevision == status2.UpdateRevision {
		t.Errorf("Expected different UpdateRevision for different resources, but got same: %s", status1.UpdateRevision)
	}
}
