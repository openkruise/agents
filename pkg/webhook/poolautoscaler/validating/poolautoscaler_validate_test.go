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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestValidatePoolAutoscalerSpec(t *testing.T) {
	tests := []struct {
		name        string
		spec        agentsv1alpha1.PoolAutoscalerSpec
		expectError string
	}{
		{
			name: "valid spec with capacity policy",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 50,
				MinReplicas: 5,
				CapacityPolicy: &agentsv1alpha1.CapacityPolicy{
					TargetAvailable: intstr.FromInt32(10),
				},
			},
			expectError: "",
		},
		{
			name: "valid spec with cron policies",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 100,
				CronPolicies: []agentsv1alpha1.CronScalingPolicy{
					{Name: "scale-up", Schedule: "0 8 * * *", TargetReplicas: 50},
					{Name: "scale-down", Schedule: "0 20 * * *", TargetReplicas: 10},
				},
			},
			expectError: "",
		},
		{
			name: "missing scaleTargetRef kind",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Name: "my-pool",
				},
				MaxReplicas: 50,
			},
			expectError: "kind is required",
		},
		{
			name: "missing scaleTargetRef name",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
				},
				MaxReplicas: 50,
			},
			expectError: "name is required",
		},
		{
			name: "unsupported kind",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "my-deploy",
				},
				MaxReplicas: 50,
			},
			expectError: "only SandboxSet is supported",
		},
		{
			name: "maxReplicas must be positive",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 0,
			},
			expectError: "must be greater than 0",
		},
		{
			name: "minReplicas greater than maxReplicas",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 10,
				MinReplicas: 20,
			},
			expectError: "must be < maxReplicas",
		},
		{
			name: "cron and capacity policies coexist - valid",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 50,
				CronPolicies: []agentsv1alpha1.CronScalingPolicy{
					{Name: "up", Schedule: "0 8 * * *", TargetReplicas: 20},
				},
				CapacityPolicy: &agentsv1alpha1.CapacityPolicy{
					TargetAvailable: intstr.FromInt32(10),
				},
			},
			expectError: "",
		},
		{
			name: "duplicate cron policy names",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 50,
				CronPolicies: []agentsv1alpha1.CronScalingPolicy{
					{Name: "same-name", Schedule: "0 8 * * *", TargetReplicas: 20},
					{Name: "same-name", Schedule: "0 20 * * *", TargetReplicas: 5},
				},
			},
			expectError: "Duplicate value",
		},
		{
			name: "cron policy missing name",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 50,
				CronPolicies: []agentsv1alpha1.CronScalingPolicy{
					{Name: "", Schedule: "0 8 * * *", TargetReplicas: 20},
				},
			},
			expectError: "name is required",
		},
		{
			name: "cron policy missing schedule",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 50,
				CronPolicies: []agentsv1alpha1.CronScalingPolicy{
					{Name: "up", Schedule: "", TargetReplicas: 20},
				},
			},
			expectError: "schedule is required",
		},
		{
			name: "stabilization window too large",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 50,
				CapacityPolicy: &agentsv1alpha1.CapacityPolicy{
					TargetAvailable: intstr.FromInt32(10),
					ScaleUp: &agentsv1alpha1.CapacityScalingRules{
						StabilizationWindowSeconds: int32Ptr(7200),
					},
				},
			},
			expectError: "must be >= 0 and <= 3600",
		},
		{
			name: "negative stabilization window",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 50,
				CapacityPolicy: &agentsv1alpha1.CapacityPolicy{
					TargetAvailable: intstr.FromInt32(10),
					ScaleDown: &agentsv1alpha1.CapacityScalingRules{
						StabilizationWindowSeconds: int32Ptr(-1),
					},
				},
			},
			expectError: "must be >= 0 and <= 3600",
		},
		{
			name: "bounds only is valid",
			spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: "my-pool",
				},
				MaxReplicas: 50,
				MinReplicas: 5,
			},
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errList := validatePoolAutoscalerSpec(tt.spec, nil)
			if tt.expectError == "" {
				assert.Empty(t, errList, "expected no errors but got: %v", errList)
			} else {
				assert.NotEmpty(t, errList, "expected an error containing %q", tt.expectError)
				found := false
				for _, e := range errList {
					if strings.Contains(e.Error(), tt.expectError) {
						found = true
						break
					}
				}
				assert.True(t, found, "expected error containing %q, got: %v", tt.expectError, errList)
			}
		})
	}
}

func int32Ptr(v int32) *int32 {
	return &v
}

// ---------------------------------------------------------------------------
// Path and Enabled tests
// ---------------------------------------------------------------------------

func TestPath(t *testing.T) {
	h := &PoolAutoscalerValidatingHandler{}
	assert.Equal(t, "/validate-poolautoscaler", h.Path())
}

func TestEnabled(t *testing.T) {
	h := &PoolAutoscalerValidatingHandler{}
	assert.True(t, h.Enabled())
}

// ---------------------------------------------------------------------------
// Handle tests
// ---------------------------------------------------------------------------

func TestHandle(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	decoder := admission.NewDecoder(scheme)

	validPA := &agentsv1alpha1.PoolAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pa", Namespace: "default"},
		Spec: agentsv1alpha1.PoolAutoscalerSpec{
			ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
				Kind: "SandboxSet",
				Name: "test-sbs",
			},
			MaxReplicas: 50,
		},
	}

	invalidPA := &agentsv1alpha1.PoolAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pa", Namespace: "default"},
		Spec: agentsv1alpha1.PoolAutoscalerSpec{
			ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
				Kind: "SandboxSet",
				Name: "test-sbs",
			},
			MaxReplicas: 0, // invalid: must be > 0
		},
	}

	tests := []struct {
		name          string
		pa            *agentsv1alpha1.PoolAutoscaler
		rawBytes      []byte // if non-nil, use these raw bytes instead of encoding pa
		expectAllowed bool
		expectCode    int32 // 0 means do not check
	}{
		{
			name:          "valid PoolAutoscaler - allowed",
			pa:            validPA,
			expectAllowed: true,
		},
		{
			name:          "invalid PoolAutoscaler - denied with 422",
			pa:            invalidPA,
			expectAllowed: false,
			expectCode:    422,
		},
		{
			name:          "decode error - returns 400",
			rawBytes:      []byte("invalid json"),
			expectAllowed: false,
			expectCode:    400,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&agentsv1alpha1.PoolAutoscaler{}, "spec.scaleTargetRef.name", func(obj client.Object) []string {
					pa := obj.(*agentsv1alpha1.PoolAutoscaler)
					if pa.Spec.ScaleTargetRef.Name == "" {
						return nil
					}
					return []string{pa.Spec.ScaleTargetRef.Name}
				}).Build()
			h := &PoolAutoscalerValidatingHandler{
				Client:  fc,
				Decoder: decoder,
			}

			var raw []byte
			if tt.rawBytes != nil {
				raw = tt.rawBytes
			} else {
				var err error
				raw, err = json.Marshal(tt.pa)
				require.NoError(t, err)
			}

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Object: runtime.RawExtension{Raw: raw},
				},
			}

			resp := h.Handle(context.Background(), req)
			assert.Equal(t, tt.expectAllowed, resp.Allowed)
			if tt.expectCode != 0 {
				require.NotNil(t, resp.Result)
				assert.Equal(t, tt.expectCode, resp.Result.Code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// validateOneToOne tests
// ---------------------------------------------------------------------------

func TestValidateOneToOne(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)

	makePA := func(name, sbsName string) *agentsv1alpha1.PoolAutoscaler {
		return &agentsv1alpha1.PoolAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: agentsv1alpha1.PoolAutoscalerSpec{
				ScaleTargetRef: agentsv1alpha1.CrossVersionObjectReference{
					Kind: "SandboxSet",
					Name: sbsName,
				},
				MaxReplicas: 50,
			},
		}
	}

	tests := []struct {
		name        string
		existingPAs []client.Object
		targetPA    *agentsv1alpha1.PoolAutoscaler
		expectError string
	}{
		{
			name:        "no conflict - empty list",
			existingPAs: nil,
			targetPA:    makePA("test-pa", "test-sbs"),
			expectError: "",
		},
		{
			name: "conflict - another PA targeting same SandboxSet",
			existingPAs: []client.Object{
				makePA("other-pa", "test-sbs"),
			},
			targetPA:    makePA("test-pa", "test-sbs"),
			expectError: "already managed by PoolAutoscaler",
		},
		{
			name: "skip self on update - no conflict",
			existingPAs: []client.Object{
				makePA("test-pa", "test-sbs"),
			},
			targetPA:    makePA("test-pa", "test-sbs"),
			expectError: "",
		},
		{
			name: "different SandboxSet targets - no conflict",
			existingPAs: []client.Object{
				makePA("other-pa", "other-sbs"),
			},
			targetPA:    makePA("test-pa", "test-sbs"),
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.existingPAs...).
				WithIndex(&agentsv1alpha1.PoolAutoscaler{}, "spec.scaleTargetRef.name", func(obj client.Object) []string {
					pa := obj.(*agentsv1alpha1.PoolAutoscaler)
					if pa.Spec.ScaleTargetRef.Name == "" {
						return nil
					}
					return []string{pa.Spec.ScaleTargetRef.Name}
				}).Build()
			h := &PoolAutoscalerValidatingHandler{Client: fc}
			errList := h.validateOneToOne(context.Background(), tt.targetPA)
			if tt.expectError == "" {
				assert.Empty(t, errList)
			} else {
				require.NotEmpty(t, errList)
				found := false
				for _, e := range errList {
					if strings.Contains(e.Error(), tt.expectError) {
						found = true
						break
					}
				}
				assert.True(t, found, "expected error containing %q, got: %v", tt.expectError, errList)
			}
		})
	}

	t.Run("client list error - returns InternalError", func(t *testing.T) {
		// Use a scheme without agentsv1alpha1 to cause a List error
		bareScheme := runtime.NewScheme()
		_ = clientgoscheme.AddToScheme(bareScheme)
		fc := fake.NewClientBuilder().WithScheme(bareScheme).Build()
		h := &PoolAutoscalerValidatingHandler{Client: fc}
		targetPA := makePA("test-pa", "test-sbs")
		errList := h.validateOneToOne(context.Background(), targetPA)
		assert.NotEmpty(t, errList)
		found := false
		for _, e := range errList {
			if strings.Contains(e.Error(), "failed to list PoolAutoscalers") {
				found = true
				break
			}
		}
		assert.True(t, found, "expected internal error about failed list, got: %v", errList)
	})
}
