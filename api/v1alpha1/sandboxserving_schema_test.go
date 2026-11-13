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

package v1alpha1_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"

	agents "github.com/openkruise/agents/api/v1alpha1"
)

func TestSandboxServingSchema(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/agents.kruise.io_sandboxservings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var external apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &external); err != nil {
		t.Fatal(err)
	}
	var crd apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&external, &crd, nil); err != nil {
		t.Fatal(err)
	}
	crd.Status.StoredVersions = []string{"v1alpha1"}
	if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &crd); len(errs) > 0 {
		t.Fatalf("CRD admission: %v", errs)
	}
	scale := external.Spec.Versions[0].Subresources.Scale
	if scale == nil || scale.SpecReplicasPath != ".spec.replicas" || scale.StatusReplicasPath != ".status.replicas" || scale.LabelSelectorPath == nil || *scale.LabelSelectorPath != ".status.selector" {
		t.Fatalf("invalid scale mapping: %+v", scale)
	}
	root := crd.Spec.Validation.OpenAPIV3Schema
	structural, err := schema.NewStructural(root)
	if err != nil {
		t.Fatal(err)
	}
	openapi, _, err := validation.NewSchemaValidator(root)
	if err != nil {
		t.Fatal(err)
	}
	celValidator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	sampleData, err := os.ReadFile("../../config/samples/agents_v1alpha1_sandboxserving.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var sample map[string]interface{}
	if err := yaml.Unmarshal(sampleData, &sample); err != nil {
		t.Fatal(err)
	}
	podTemplate := map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "test"}}, "spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "app", "image": "test:v1"}}}}
	base := map[string]interface{}{"spec": map[string]interface{}{"provision": map[string]interface{}{"sandboxTemplate": map[string]interface{}{"template": podTemplate}}, "ports": []interface{}{int64(8080)}}}
	defaulting.Default(base, structural)
	base["metadata"] = map[string]interface{}{"name": "test"}
	pruned := runtime.DeepCopyJSON(base)
	pruning.Prune(pruned, structural, true)
	metadata := pruned["spec"].(map[string]interface{})["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["template"].(map[string]interface{})["metadata"].(map[string]interface{})
	if _, exists := metadata["labels"]; !exists {
		t.Fatal("structural pruning discarded PodTemplate metadata.labels")
	}
	spec := base["spec"].(map[string]interface{})
	if spec["replicas"] != int64(1) && spec["replicas"] != float64(1) {
		t.Fatalf("replicas default=%v", spec["replicas"])
	}
	if spec["sessionPolicy"].(map[string]interface{})["affinity"] != "BestEffort" || spec["updateStrategy"].(map[string]interface{})["maxSurge"] != "25%" {
		t.Fatalf("missing defaults: %v", spec)
	}
	for _, tt := range []struct {
		name    string
		mutate  func(map[string]interface{})
		update  bool
		invalid bool
	}{
		{name: "valid", mutate: func(map[string]interface{}) {}},
		{name: "checked-in sample", mutate: func(s map[string]interface{}) {
			clear(s)
			maps.Copy(s, sample["spec"].(map[string]interface{}))
		}},
		{name: "zero", mutate: func(s map[string]interface{}) { s["replicas"] = int64(0) }},
		{name: "missing nested template is handled by the controller", mutate: func(s map[string]interface{}) {
			delete(s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{}), "template")
		}},
		{name: "empty buildOptions", invalid: true, mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["buildOptions"] = map[string]interface{}{}
		}},
		{name: "configured buildOptions", invalid: true, mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["buildOptions"] = map[string]interface{}{"fromTemplate": "base"}
		}},
		{name: "unsupported backend", invalid: true, mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["backend"] = "other"
		}},
		{name: "valid PVC and probe policy", mutate: func(s map[string]interface{}) {
			template := s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})
			template["volumeClaimTemplates"] = []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "data", "labels": map[string]interface{}{"storage": "test"}}, "spec": map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}, "resources": map[string]interface{}{"requests": map[string]interface{}{"storage": "1Gi"}}}}}
			template["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["volumeMounts"] = []interface{}{map[string]interface{}{"name": "data", "mountPath": "/data"}}
			template["probes"] = []interface{}{map[string]interface{}{"name": "idle", "exec": map[string]interface{}{"command": []interface{}{"check"}}}}
			template["autoPausePolicy"] = map[string]interface{}{"pause": map[string]interface{}{"whenProbedIdleState": map[string]interface{}{"probe": "idle", "messageRegex": "^idle$", "thresholdDuration": "1m"}}}
		}},
		{name: "valid pause resume lifecycle", mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["lifecycle"] = map[string]interface{}{"prePause": map[string]interface{}{"exec": map[string]interface{}{"command": []interface{}{"flush"}}, "timeoutSeconds": int64(1)}, "postResume": map[string]interface{}{"exec": map[string]interface{}{"command": []interface{}{"restore"}}, "timeoutSeconds": int64(60)}}
		}},
		{name: "upgrade lifecycle forbidden", invalid: true, mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["lifecycle"] = map[string]interface{}{"preUpgrade": map[string]interface{}{"exec": map[string]interface{}{"command": []interface{}{"upgrade"}}}}
		}},
		{name: "pause lifecycle requires command", invalid: true, mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["lifecycle"] = map[string]interface{}{"prePause": map[string]interface{}{"exec": map[string]interface{}{"command": []interface{}{}}}}
		}},
		{name: "resume lifecycle timeout limit", invalid: true, mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["lifecycle"] = map[string]interface{}{"postResume": map[string]interface{}{"exec": map[string]interface{}{"command": []interface{}{"restore"}}, "timeoutSeconds": int64(61)}}
		}},
		{name: "pause lifecycle explicit zero timeout", invalid: true, mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["lifecycle"] = map[string]interface{}{"prePause": map[string]interface{}{"exec": map[string]interface{}{"command": []interface{}{"flush"}}, "timeoutSeconds": int64(0)}}
		}},
		{name: "negative replicas", mutate: func(s map[string]interface{}) { s["replicas"] = int64(-1) }, invalid: true},
		{name: "missing provision", mutate: func(s map[string]interface{}) { delete(s, "provision") }, invalid: true},
		{name: "empty provision", mutate: func(s map[string]interface{}) { s["provision"] = map[string]interface{}{} }, invalid: true},
		{name: "claim", mutate: func(s map[string]interface{}) {
			s["provision"] = map[string]interface{}{"sandboxSetRef": map[string]interface{}{"name": "pool"}}
		}, invalid: true},
		{name: "both", mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxSetRef"] = map[string]interface{}{"name": "pool"}
		}, invalid: true},
		{name: "capacity zero", mutate: func(s map[string]interface{}) {
			s["sessionPolicy"].(map[string]interface{})["maxSessionsPerSandbox"] = int64(0)
		}, invalid: true},
		{name: "capacity 512", mutate: func(s map[string]interface{}) {
			s["sessionPolicy"].(map[string]interface{})["maxSessionsPerSandbox"] = int64(512)
		}},
		{name: "capacity 513", mutate: func(s map[string]interface{}) {
			s["sessionPolicy"].(map[string]interface{})["maxSessionsPerSandbox"] = int64(513)
		}, invalid: true},
		{name: "duplicate ports", mutate: func(s map[string]interface{}) { s["ports"] = []interface{}{int64(80), int64(80)} }, invalid: true},
		{name: "empty ports", mutate: func(s map[string]interface{}) { s["ports"] = []interface{}{} }, invalid: true},
		{name: "port zero", mutate: func(s map[string]interface{}) { s["ports"] = []interface{}{int64(0)} }, invalid: true},
		{name: "port too high", mutate: func(s map[string]interface{}) { s["ports"] = []interface{}{int64(65536)} }, invalid: true},
		{name: "negative idle", mutate: func(s map[string]interface{}) { s["sessionPolicy"].(map[string]interface{})["pauseAfterIdle"] = "-1m" }, invalid: true},
		{name: "zero idle", mutate: func(s map[string]interface{}) { s["sessionPolicy"].(map[string]interface{})["expireAfterIdle"] = "0s" }, invalid: true},
		{name: "valid idle declaration", mutate: func(s map[string]interface{}) { s["sessionPolicy"].(map[string]interface{})["pauseAfterIdle"] = "30m" }},
		{name: "image immutable", update: true, mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["image"] = "test:v2"
		}, invalid: true},
		{name: "metadata immutable", update: true, mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["template"].(map[string]interface{})["metadata"].(map[string]interface{})["labels"].(map[string]interface{})["app"] = "changed"
		}, invalid: true},
		{name: "template field addition immutable", update: true, mutate: func(s map[string]interface{}) {
			s["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})["persistentContents"] = []interface{}{"filesystem"}
		}, invalid: true},
		{name: "scale policy ports mutable", update: true, mutate: func(s map[string]interface{}) {
			s["replicas"] = int64(3)
			s["ports"] = []interface{}{int64(8081)}
			s["sessionPolicy"].(map[string]interface{})["affinity"] = "Strict"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := runtime.DeepCopyJSON(base)
			tt.mutate(obj["spec"].(map[string]interface{}))
			pruning.Prune(obj, structural, true)
			errs := validation.ValidateCustomResource(field.NewPath(""), obj, openapi)
			errs = append(errs, listtype.ValidateListSetsAndMaps(field.NewPath(""), structural, obj)...)
			var old interface{}
			if tt.update {
				old = base
			}
			celErrs, _ := celValidator.Validate(context.Background(), field.NewPath(""), structural, obj, old, celconfig.RuntimeCELCostBudget)
			encoded, err := json.Marshal(map[string]interface{}{"apiVersion": agents.GroupVersion.String(), "kind": "SandboxServingList", "items": []interface{}{obj}})
			if err != nil {
				t.Fatal(err)
			}
			var typed agents.SandboxServingList
			decodeErr := json.Unmarshal(encoded, &typed)
			invalid := len(errs)+len(celErrs) > 0
			if invalid != tt.invalid {
				t.Fatalf("invalid=%v want=%v, schema=%v CEL=%v typedListDecode=%v", invalid, tt.invalid, errs, celErrs, decodeErr)
			}
			if !invalid && decodeErr != nil {
				t.Fatalf("schema-admitted Serving poisoned typed list decoding: %v", decodeErr)
			}
		})
	}
	for key, value := range map[string]interface{}{
		"pauseStrategy":        map[string]interface{}{"type": "Stop"},
		"persistentContents":   []interface{}{"filesystem"},
		"runtimes":             []interface{}{map[string]interface{}{"name": "agent-runtime"}},
		"volumeClaimTemplates": []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "data"}, "spec": map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}}}},
		"probes":               []interface{}{map[string]interface{}{"name": "idle", "exec": map[string]interface{}{"command": []interface{}{"check"}}}},
		"autoPausePolicy":      map[string]interface{}{"resume": map[string]interface{}{"onIngressTraffic": map[string]interface{}{}}},
		"lifecycle":            map[string]interface{}{"prePause": map[string]interface{}{"exec": map[string]interface{}{"command": []interface{}{"flush"}}}},
	} {
		t.Run(key+" creation input immutability", func(t *testing.T) {
			old := runtime.DeepCopyJSON(base)
			old["spec"].(map[string]interface{})["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})[key] = value
			errs, _ := celValidator.Validate(context.Background(), field.NewPath(""), structural, base, old, celconfig.RuntimeCELCostBudget)
			if len(errs) == 0 {
				t.Fatalf("unexpected creation-input removal validation: %v", errs)
			}
			errs, _ = celValidator.Validate(context.Background(), field.NewPath(""), structural, old, base, celconfig.RuntimeCELCostBudget)
			if len(errs) == 0 {
				t.Fatalf("unexpected creation-input addition validation: %v", errs)
			}
			if key == "volumeClaimTemplates" {
				changed := runtime.DeepCopyJSON(old)
				template := changed["spec"].(map[string]interface{})["provision"].(map[string]interface{})["sandboxTemplate"].(map[string]interface{})
				claim := template[key].([]interface{})[0].(map[string]interface{})
				claim["spec"].(map[string]interface{})["accessModes"] = []interface{}{"ReadWriteMany"}
				errs, _ = celValidator.Validate(context.Background(), field.NewPath(""), structural, changed, old, celconfig.RuntimeCELCostBudget)
				if len(errs) == 0 {
					t.Fatal("accepted modification inside a PVC template")
				}
			}
		})
	}
}
