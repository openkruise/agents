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

package validating

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/openkruise/agents/api/v1alpha1"
)

// newExecProbe builds a minimal valid exec probe for the validation cases.
func newExecProbe(name string, command ...string) v1alpha1.Probe {
	return v1alpha1.Probe{
		Name: name,
		Probe: corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: command},
			},
		},
	}
}

func TestSandboxSetValidatingHandler_Handle(t *testing.T) {
	// Add v1alpha1 to scheme
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	tests := []struct {
		name       string
		sandboxSet *v1alpha1.SandboxSet
		// operation defaults to Create when empty.
		operation    admissionv1.Operation
		expectAllow  bool
		expectError  bool
		errorMessage string
	}{
		{
			name: "Valid SandboxSet",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app": "test",
								},
							},
							Spec: corev1.PodSpec{
								RestartPolicy:                 corev1.RestartPolicyAlways,
								DNSPolicy:                     corev1.DNSClusterFirst,
								TerminationGracePeriodSeconds: new(int64),
								Containers: []corev1.Container{
									{
										Name:                     "test",
										Image:                    "nginx:latest",
										ImagePullPolicy:          corev1.PullAlways,
										TerminationMessagePolicy: corev1.TerminationMessageReadFile,
									},
								},
							},
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Valid SandboxSet With VolumeClaimTemplate",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app": "test",
								},
							},
							Spec: corev1.PodSpec{
								RestartPolicy:                 corev1.RestartPolicyAlways,
								DNSPolicy:                     corev1.DNSClusterFirst,
								TerminationGracePeriodSeconds: new(int64),
								Containers: []corev1.Container{
									{
										Name:                     "test",
										Image:                    "nginx:latest",
										ImagePullPolicy:          corev1.PullAlways,
										TerminationMessagePolicy: corev1.TerminationMessageReadFile,
										VolumeMounts: []corev1.VolumeMount{
											{
												Name:      "test-pvc",
												MountPath: "/data",
											},
										},
									},
								},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "test-pvc",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{
										corev1.ReadWriteOnce,
									},
								},
							},
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Invalid SandboxSet with unmounted VolumeClaimTemplate",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app": "test",
								},
							},
							Spec: corev1.PodSpec{
								RestartPolicy:                 corev1.RestartPolicyAlways,
								DNSPolicy:                     corev1.DNSClusterFirst,
								TerminationGracePeriodSeconds: new(int64),
								Containers: []corev1.Container{
									{
										Name:                     "test",
										Image:                    "nginx:latest",
										ImagePullPolicy:          corev1.PullAlways,
										TerminationMessagePolicy: corev1.TerminationMessageReadFile,
									},
								},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "test-pvc",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{
										corev1.ReadWriteOnce,
									},
								},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "must be mounted by at least one container",
		},
		{
			name: "Invalid SandboxSet with VolumeClaimTemplate and mismatched volume mount",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app": "test",
								},
							},
							Spec: corev1.PodSpec{
								RestartPolicy:                 corev1.RestartPolicyAlways,
								DNSPolicy:                     corev1.DNSClusterFirst,
								TerminationGracePeriodSeconds: new(int64),
								Containers: []corev1.Container{
									{
										Name:                     "test",
										Image:                    "nginx:latest",
										ImagePullPolicy:          corev1.PullAlways,
										TerminationMessagePolicy: corev1.TerminationMessageReadFile,
										VolumeMounts: []corev1.VolumeMount{
											{
												Name:      "other-pvc",
												MountPath: "/data",
											},
										},
									},
								},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "test-pvc",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{
										corev1.ReadWriteOnce,
									},
								},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "must be mounted by at least one container",
		},
		{
			name: "Valid SandboxSet with templateref",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-sbs",
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Invalid name",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "TEST-SBS",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app": "test",
								},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test",
										Image: "nginx:latest",
									},
								},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "subdomain must consist of lower case alphanumeric characters, '-' or '.'",
		},
		{
			name: "Negative replicas",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: -1, // Negative replicas are invalid
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									v1alpha1.E2BPrefix + "test": "value", // Template internal prefix labels are invalid
								},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "replicas cannot be negative",
		},
		{
			name: "Label with internal prefix",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.E2BPrefix + "test": "value", // Internal prefix labels are invalid
					},
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									v1alpha1.E2BPrefix + "test": "value", // Template internal prefix labels are invalid
								},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "label cannot start with " + v1alpha1.E2BPrefix,
		},
		{
			name: "Template label with internal prefix",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									v1alpha1.E2BPrefix + "test": "value", // Template internal prefix labels are invalid
								},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "label cannot start with " + v1alpha1.E2BPrefix,
		},
		{
			name: "SandboxSet with both templateRef and podTemplate",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									v1alpha1.E2BPrefix + "test": "value", // Template internal prefix labels are invalid
								},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "templateRef and podtemplate is mutual exclusive",
		},
		{
			name: "Valid MaxUnavailable - integer value",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					ScaleStrategy: v1alpha1.SandboxSetScaleStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 3},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Valid MaxUnavailable - percentage value 50%",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					ScaleStrategy: v1alpha1.SandboxSetScaleStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "50%"},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Valid MaxUnavailable - 0 value",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					ScaleStrategy: v1alpha1.SandboxSetScaleStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Valid MaxUnavailable - 100% value",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					ScaleStrategy: v1alpha1.SandboxSetScaleStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "100%"},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Invalid MaxUnavailable - negative integer",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					ScaleStrategy: v1alpha1.SandboxSetScaleStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: -5},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "must be >= 0",
		},
		{
			name: "Invalid MaxUnavailable - negative percentage",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					ScaleStrategy: v1alpha1.SandboxSetScaleStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "-10%"},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: `must be a percentage in the form "<number>%"`,
		},
		{
			name: "Invalid MaxUnavailable - exceeds 100%",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					ScaleStrategy: v1alpha1.SandboxSetScaleStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "150%"},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "must be within [0%, 100%]",
		},
		{
			name: "Invalid MaxUnavailable - invalid format (not a percentage)",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					ScaleStrategy: v1alpha1.SandboxSetScaleStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "abc"},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: `must be a percentage in the form "<number>%"`,
		},
		{
			name: "Invalid MaxUnavailable - invalid percentage format",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					ScaleStrategy: v1alpha1.SandboxSetScaleStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "50.5%"},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: `must be a percentage in the form "<number>%"`,
		},
		{
			name: "Valid UpdateStrategy.MaxUnavailable - percentage value",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					UpdateStrategy: v1alpha1.SandboxSetUpdateStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Valid UpdateStrategy.MaxUnavailable - integer value",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					UpdateStrategy: v1alpha1.SandboxSetUpdateStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 3},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Invalid UpdateStrategy.MaxUnavailable - invalid format",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 10,
					UpdateStrategy: v1alpha1.SandboxSetUpdateStrategy{
						MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "invalid"},
					},
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{
							Name: "test-template",
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: `must be a percentage in the form "<number>%"`,
		},
		{
			name: "Valid probes and autoPausePolicy",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "test-template"},
					},
					Probes: []v1alpha1.Probe{
						newExecProbe("idle", "cat", "/tmp/idle"),
						newExecProbe("schedule", "cat", "/tmp/schedule"),
					},
					AutoPausePolicy: &v1alpha1.AutoPausePolicy{
						Pause: &v1alpha1.PausePolicy{
							WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{
								Probe:             "idle",
								MessageRegex:      "^idle$",
								ThresholdDuration: &metav1.Duration{Duration: 5 * time.Minute},
							},
						},
						Resume: &v1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &v1alpha1.ProbedScheduleTimeRule{
								Probe:      "schedule",
								TimeFormat: v1alpha1.ProbeTimeFormatUnix,
								LeadTime:   &metav1.Duration{Duration: time.Minute},
							},
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Valid ingress traffic autoPausePolicy without probes",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "test-template"},
					},
					AutoPausePolicy: &v1alpha1.AutoPausePolicy{
						Resume: &v1alpha1.ResumePolicy{
							OnIngressTraffic: &v1alpha1.IngressTrafficRule{},
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Invalid probe - duplicate name",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "test-template"},
					},
					Probes: []v1alpha1.Probe{
						newExecProbe("idle", "cat", "/tmp/a"),
						newExecProbe("idle", "cat", "/tmp/b"),
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "Duplicate value",
		},
		{
			name: "Invalid probe - non-exec handler",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "test-template"},
					},
					Probes: []v1alpha1.Probe{
						{
							Name: "http",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Path: "/healthz"},
								},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "Unsupported value: \"httpGet\": supported values: \"exec\"",
		},
		{
			name: "Invalid autoPausePolicy - pause rule references undefined probe",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "test-template"},
					},
					Probes: []v1alpha1.Probe{newExecProbe("idle", "cat", "/tmp/idle")},
					AutoPausePolicy: &v1alpha1.AutoPausePolicy{
						Pause: &v1alpha1.PausePolicy{
							WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{
								Probe:             "missing",
								MessageRegex:      "^idle$",
								ThresholdDuration: &metav1.Duration{Duration: time.Minute},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "must reference a probe name defined in spec.probes",
		},
		{
			name: "Invalid autoPausePolicy - messageRegex does not compile",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "test-template"},
					},
					Probes: []v1alpha1.Probe{newExecProbe("idle", "cat", "/tmp/idle")},
					AutoPausePolicy: &v1alpha1.AutoPausePolicy{
						Pause: &v1alpha1.PausePolicy{
							WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{
								Probe:             "idle",
								MessageRegex:      "([a-z",
								ThresholdDuration: &metav1.Duration{Duration: time.Minute},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "error parsing regexp",
		},
		{
			name: "Invalid autoPausePolicy - thresholdDuration missing",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "test-template"},
					},
					Probes: []v1alpha1.Probe{newExecProbe("idle", "cat", "/tmp/idle")},
					AutoPausePolicy: &v1alpha1.AutoPausePolicy{
						Pause: &v1alpha1.PausePolicy{
							WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{
								Probe:        "idle",
								MessageRegex: "^idle$",
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "thresholdDuration is required",
		},
		{
			// The webhook is registered for update as well, so a probe change that
			// would have been rejected at create cannot slip in through an update.
			name:      "Invalid probe on update - undefined probe reference",
			operation: admissionv1.Update,
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "test-template"},
					},
					Probes: []v1alpha1.Probe{newExecProbe("idle", "cat", "/tmp/idle")},
					AutoPausePolicy: &v1alpha1.AutoPausePolicy{
						Pause: &v1alpha1.PausePolicy{
							WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{
								Probe:             "renamed",
								MessageRegex:      "^idle$",
								ThresholdDuration: &metav1.Duration{Duration: time.Minute},
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "must reference a probe name defined in spec.probes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)

			// Create fake client
			objs := []runtime.Object{}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(objs...).Build()

			// Create decoder
			decoder := admission.NewDecoder(scheme.Scheme)

			// Create handler
			handler := &SandboxSetValidatingHandler{
				Client:  fakeClient,
				Decoder: decoder,
			}

			// Construct admission request
			sbsRaw, err := json.Marshal(tt.sandboxSet)
			require.NoError(t, err)

			operation := tt.operation
			if operation == "" {
				operation = admissionv1.Create
			}
			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Operation: operation,
					Object: runtime.RawExtension{
						Raw: sbsRaw,
					},
					OldObject: runtime.RawExtension{
						Raw: sbsRaw,
					},
				},
			}

			response := handler.Handle(context.TODO(), req)

			// Verify results
			if tt.expectAllow {
				t.Log(response.String())
				g.Expect(response.Allowed).To(gomega.BeTrue())
			} else {
				g.Expect(response.Allowed).To(gomega.BeFalse())
			}

			if tt.expectError {
				g.Expect(response.Result).NotTo(gomega.BeNil())
				g.Expect(response.Result.Message).To(gomega.ContainSubstring(tt.errorMessage))
			}
		})
	}
}

func TestSandboxSetManagedTemplateAdmission(t *testing.T) {
	for _, tt := range []struct {
		name         string
		mutate       func(*v1alpha1.SandboxTemplate)
		missingBuild bool
		getError     bool
		malformed    bool
		listError    error
		code         int32
		message      string
	}{
		{name: "Ready static Version"},
		{name: "malformed request", malformed: true, code: 400, message: "unexpected"},
		{name: "name occupied by template group", code: 409, message: "occupied", mutate: func(v *v1alpha1.SandboxTemplate) { v.Labels[v1alpha1.LabelTemplateName] = "pool" }},
		{name: "name lookup unavailable", listError: apierrors.NewServiceUnavailable("cache unavailable"), code: 503, message: "check pool name"},
		{name: "unsupported template kind does not occupy name", missingBuild: true, listError: &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: "SandboxTemplate"}}},
		{name: "Draft", code: 422, message: "only Ready static Versions", mutate: func(v *v1alpha1.SandboxTemplate) { v.Status.Phase = v1alpha1.SandboxTemplatePhasePending }},
		{name: "Building", code: 422, message: "only Ready static Versions", mutate: func(v *v1alpha1.SandboxTemplate) { v.Status.Phase = v1alpha1.SandboxTemplatePhaseBuilding }},
		{name: "stale Ready", code: 422, message: "only Ready static Versions", mutate: func(v *v1alpha1.SandboxTemplate) { v.Status.ObservedGeneration = 0 }},
		{name: "captured Version", code: 422, message: "only Ready static Versions", mutate: func(v *v1alpha1.SandboxTemplate) {
			v.Status.Artifacts = []v1alpha1.SandboxTemplateArtifact{{Type: "checkpoint", Name: "capture", Value: "backend-id"}}
		}},
		{name: "deleting Version", code: 422, message: "only Ready static Versions", mutate: func(v *v1alpha1.SandboxTemplate) {
			v.DeletionTimestamp = ptr.To(metav1.Now())
			v.Finalizers = []string{"test/retain"}
		}},
		{name: "unmanaged reference", mutate: func(v *v1alpha1.SandboxTemplate) {
			v.Labels = nil
			v.Status = v1alpha1.SandboxTemplateStatus{}
		}},
		{name: "ordinary Snapshot reference", mutate: func(v *v1alpha1.SandboxTemplate) {
			v.Labels = nil
			v.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Checkpoint", Name: "capture", UID: "checkpoint-uid", Controller: ptr.To(true)}}
			v.Status = v1alpha1.SandboxTemplateStatus{}
		}},
		{name: "missing reference retains existing behavior", missingBuild: true},
		{name: "reference lookup unavailable", getError: true, code: 503, message: "read referenced template"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(s))
			require.NoError(t, v1alpha1.AddToScheme(s))
			version := &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "st-abcdefghijklmnopqrst", Namespace: "default", Generation: 1,
					Labels: map[string]string{v1alpha1.LabelTemplateName: "python-agent"}},
				Status: v1alpha1.SandboxTemplateStatus{Phase: v1alpha1.SandboxTemplatePhaseReady, ObservedGeneration: 1},
			}
			if tt.mutate != nil {
				tt.mutate(version)
			}
			builder := fake.NewClientBuilder().WithScheme(s)
			if !tt.missingBuild {
				builder = builder.WithObjects(version)
			}
			builder = builder.WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if tt.listError != nil {
						return tt.listError
					}
					return c.List(ctx, list, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if tt.getError && key.Name == version.Name {
						return apierrors.NewServiceUnavailable("cache unavailable")
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			pool := &v1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: version.Namespace}, Spec: v1alpha1.SandboxSetSpec{EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{TemplateRef: &v1alpha1.SandboxTemplateRef{Name: version.Name}}}}
			raw, err := json.Marshal(pool)
			require.NoError(t, err)
			if tt.malformed {
				raw = []byte("{")
			}
			handler := &SandboxSetValidatingHandler{Client: builder.Build(), Decoder: admission.NewDecoder(s)}
			response := handler.Handle(t.Context(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}}})
			if tt.code == 0 {
				require.True(t, response.Allowed, "%v", response.Result)
			} else {
				require.False(t, response.Allowed)
				require.Equal(t, tt.code, response.Result.Code)
				require.Contains(t, response.Result.Message, tt.message)
			}
		})
	}
}
