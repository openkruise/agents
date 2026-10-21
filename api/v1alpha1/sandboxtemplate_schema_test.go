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
	"os"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestSandboxTemplateCRDSchema(t *testing.T) {
	if agentsv1alpha1.SandboxTemplateBackendKubernetes != "k8s" {
		t.Fatalf("kubernetes backend wire value = %q", agentsv1alpha1.SandboxTemplateBackendKubernetes)
	}
	if agentsv1alpha1.SandboxTemplateBackendSubstrate != "substrate" {
		t.Fatalf("substrate backend wire value = %q", agentsv1alpha1.SandboxTemplateBackendSubstrate)
	}
	if agentsv1alpha1.AnnotationTemplateAutoWarm != "agents.kruise.io/auto-warm" {
		t.Fatalf("auto-warm annotation = %q", agentsv1alpha1.AnnotationTemplateAutoWarm)
	}
	if agentsv1alpha1.AnnotationTemplateWarmPoolSize != "agents.kruise.io/warmpool-size" {
		t.Fatalf("warm-pool-size annotation = %q", agentsv1alpha1.AnnotationTemplateWarmPoolSize)
	}
	if agentsv1alpha1.LabelTemplateAutoWarm != agentsv1alpha1.AnnotationTemplateAutoWarm {
		t.Fatalf("auto-warm label %q differs from annotation %q", agentsv1alpha1.LabelTemplateAutoWarm, agentsv1alpha1.AnnotationTemplateAutoWarm)
	}
	data, err := os.ReadFile("../../config/crd/bases/agents.kruise.io_sandboxtemplates.yaml")
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
	if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &crd); len(errs) != 0 {
		t.Fatalf("CRD admission: %v", errs)
	}

	version := external.Spec.Versions[0]
	if version.Subresources == nil || version.Subresources.Status == nil {
		t.Fatal("status subresource is missing")
	}

	root := crd.Spec.Validation.OpenAPIV3Schema
	spec := root.Properties["spec"]
	buildOptions, ok := spec.Properties["buildOptions"]
	if !ok {
		t.Fatal("spec.buildOptions is missing")
	}
	for _, name := range []string{"fromTemplate", "checkpoint", "imageAcceleration", "steps", "startCommand", "readyCommand"} {
		if _, ok := buildOptions.Properties[name]; !ok {
			t.Errorf("spec.buildOptions.%s is missing", name)
		}
	}
	steps := buildOptions.Properties["steps"]
	cwd := steps.Items.Schema.Properties["cwd"]
	if len(cwd.XValidations) != 1 || cwd.XValidations[0].Rule != "self == '' || self.startsWith('/')" {
		t.Fatalf("cwd absolute-path validation = %#v", cwd.XValidations)
	}

	for _, name := range []string{"templateBuildOptions", "imageCache", "businessProcess", "runCommands"} {
		if _, ok := spec.Properties[name]; ok {
			t.Errorf("legacy spec.%s must not be exposed", name)
		}
		if _, ok := buildOptions.Properties[name]; ok {
			t.Errorf("legacy spec.buildOptions.%s must not be exposed", name)
		}
	}

	status := root.Properties["status"]
	for _, name := range []string{"observedGeneration", "phase", "artifacts", "conditions"} {
		if _, ok := status.Properties[name]; !ok {
			t.Errorf("status.%s is missing", name)
		}
	}
	for _, name := range []string{"artifactRef", "message", "startTime", "readyTime"} {
		if _, ok := status.Properties[name]; ok {
			t.Errorf("legacy status.%s must not be exposed", name)
		}
	}

	structural, err := schema.NewStructural(root)
	if err != nil {
		t.Fatal(err)
	}
	obj := map[string]interface{}{
		"apiVersion": "agents.kruise.io/v1alpha1",
		"kind":       "SandboxTemplate",
		"metadata":   map[string]interface{}{"name": "python-agent"},
		"spec":       map[string]interface{}{},
	}
	defaulting.Default(obj, structural)
	got := obj["spec"].(map[string]interface{})["backend"]
	if got != "k8s" {
		t.Fatalf("default backend = %v, want k8s", got)
	}
}
