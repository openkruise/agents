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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/openkruise/agents/api/v1alpha1"
)

// minimalPodTemplate builds a pod template that passes the unrelated template
// validations, so a case can fail only on what it is testing.
func minimalPodTemplate() *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
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
	}
}

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

func TestSandboxTemplateValidatingHandler_Handle(t *testing.T) {
	// Add v1alpha1 to scheme
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	tests := []struct {
		name            string
		sandboxTemplate *v1alpha1.SandboxTemplate
		expectAllow     bool
		expectError     bool
		errorMessage    string
	}{
		{
			name: "Valid SandboxSet",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
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
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Invalid name",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "TEST-SBS",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
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
			expectAllow:  false,
			expectError:  true,
			errorMessage: "subdomain must consist of lower case alphanumeric characters, '-' or '.'",
		},
		{
			name: "Valid SandboxTemplate With VolumeClaimTemplate",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbt",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
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
											Name:      "data-vol",
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
								Name: "data-vol",
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
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Label with internal prefix",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.E2BPrefix + "test": "value", // Internal prefix labels are invalid
					},
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								v1alpha1.E2BPrefix + "test": "value", // Template internal prefix labels are invalid
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
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								v1alpha1.E2BPrefix + "test": "value", // Template internal prefix labels are invalid
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
			name: "Missing template",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "template is required",
		},
		{
			// Probes and the policy propagate to every Sandbox created from this
			// template, so they are validated here even though the template itself
			// never runs a probe.
			name: "Valid probes and autoPausePolicy",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbt",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: minimalPodTemplate(),
					Probes:   []v1alpha1.Probe{newExecProbe("idle", "cat", "/tmp/idle")},
					AutoPausePolicy: &v1alpha1.AutoPausePolicy{
						Pause: &v1alpha1.PausePolicy{
							WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{
								Probe:             "idle",
								MessageRegex:      "^idle$",
								ThresholdDuration: &metav1.Duration{Duration: time.Minute},
							},
						},
					},
				},
			},
			expectAllow: true,
		},
		{
			name: "Invalid probe - duplicate name",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbt",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: minimalPodTemplate(),
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
			name: "Invalid autoPausePolicy - resume rule references undefined probe",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbt",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: minimalPodTemplate(),
					Probes:   []v1alpha1.Probe{newExecProbe("idle", "cat", "/tmp/idle")},
					AutoPausePolicy: &v1alpha1.AutoPausePolicy{
						Resume: &v1alpha1.ResumePolicy{
							WhenProbedScheduleTime: &v1alpha1.ProbedScheduleTimeRule{Probe: "schedule"},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "must reference a probe name defined in spec.probes",
		},
		{
			// The probe rules are reported even when the template itself is missing,
			// so a single apply does not have to be fixed one error at a time.
			name: "Invalid probe reported alongside missing template",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbt",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Probes: []v1alpha1.Probe{newExecProbe("idle")},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "exec command is required",
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
			handler := &ValidatingHandler{
				Client:  fakeClient,
				Decoder: decoder,
			}

			// Construct admission request
			sbsRaw, err := json.Marshal(tt.sandboxTemplate)
			require.NoError(t, err)

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Operation: admissionv1.Create,
					Object: runtime.RawExtension{
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
