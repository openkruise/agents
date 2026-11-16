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

package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func TestSandboxTemplateClaimShape(t *testing.T) {
	kind := reflect.TypeFor[SandboxTemplateClaim]()
	if kind.NumField() != 2 {
		t.Fatalf("Claim has %d fields, want metadata only", kind.NumField())
	}
	for _, forbidden := range []string{"Spec", "Status", "DefaultVersionRef", "DisplayName"} {
		if _, ok := kind.FieldByName(forbidden); ok {
			t.Fatalf("Claim exposes %s", forbidden)
		}
	}
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if _, err := scheme.New(GroupVersion.WithKind("SandboxTemplateClaim")); err != nil {
		t.Fatal(err)
	}
	claim := &SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Name: "python-agent", Namespace: "team-a", Finalizers: []string{SandboxTemplateClaimCleanupFinalizer}}}
	clone := claim.DeepCopy()
	clone.Finalizers[0] = "changed"
	if claim.Finalizers[0] != SandboxTemplateClaimCleanupFinalizer {
		t.Fatal("Claim metadata aliases its deepcopy")
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["spec"]; ok {
		t.Fatal("Claim JSON exposes spec")
	}
	if _, ok := decoded["status"]; ok {
		t.Fatal("Claim JSON exposes status")
	}
}

func TestSandboxTemplateBuildOptionsWarmPoolDeepCopy(t *testing.T) {
	original := &SandboxTemplateBuildOptions{
		FromTemplate: "legacy-base", Steps: []TemplateBuildCommand{{Command: "echo setup", Env: map[string]string{"KEY": "value"}}},
		StartCommand: &TemplateBuildCommand{Command: "start"}, ReadyCommand: &TemplateBuildCommand{Command: "ready"},
		WarmPool: &SandboxTemplateWarmPoolOptions{Enabled: ptr.To(true), Size: ptr.To(int32(2)), Autoscaler: &SandboxTemplatePoolAutoscalerOptions{MaxReplicas: 10, CapacityPolicy: &CapacityPolicy{}, CronPolicies: []CronScalingPolicy{{Name: "day", Schedule: "0 9 * * *"}}}},
	}
	clone := original.DeepCopy()
	*clone.WarmPool.Enabled = false
	*clone.WarmPool.Size = 9
	clone.WarmPool.Autoscaler.CronPolicies[0].Name = "changed"
	clone.Steps[0].Env["KEY"] = "changed"
	if !*original.WarmPool.Enabled || *original.WarmPool.Size != 2 || original.WarmPool.Autoscaler.CronPolicies[0].Name != "day" || original.Steps[0].Env["KEY"] != "value" {
		t.Fatal("nested build options alias deepcopy")
	}
	if original.StartCommand.Command != "start" || original.ReadyCommand.Command != "ready" || original.FromTemplate != "legacy-base" {
		t.Fatal("legacy build inputs were removed")
	}
	disabled := SandboxTemplateBuildOptions{WarmPool: &SandboxTemplateWarmPoolOptions{Enabled: ptr.To(false)}}
	data, err := json.Marshal(disabled)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]map[string]interface{}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if _, ok := value["warmPool"]["size"]; ok {
		t.Fatal("disabled absent size serialized as zero")
	}
}
