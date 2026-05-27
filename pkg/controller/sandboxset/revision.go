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

package sandboxset

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"

	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// buildSandboxTemplateSpec constructs the effective SandboxTemplateSpec from a
// SandboxSet, handling both inline template and templateRef cases.
// WARNING: the returned spec shares slice fields with sbs.Spec; callers must
// not mutate VolumeClaimTemplates, PersistentContents or Runtimes.
func (r *Reconciler) buildSandboxTemplateSpec(ctx context.Context, sbs *agentsv1alpha1.SandboxSet) (*agentsv1alpha1.SandboxTemplateSpec, error) {
	if sbs.Spec.TemplateRef != nil {
		tpl := &agentsv1alpha1.SandboxTemplate{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: sbs.Namespace,
			Name:      sbs.Spec.TemplateRef.Name,
		}, tpl); err != nil {
			return nil, fmt.Errorf("failed to resolve sandbox template %s/%s: %w",
				sbs.Namespace, sbs.Spec.TemplateRef.Name, err)
		}
		return &agentsv1alpha1.SandboxTemplateSpec{
			Template:             tpl.Spec.Template.DeepCopy(),
			VolumeClaimTemplates: sbs.Spec.VolumeClaimTemplates,
			PersistentContents:   sbs.Spec.PersistentContents,
			Runtimes:             sbs.Spec.Runtimes,
		}, nil
	}

	if sbs.Spec.Template == nil {
		return nil, fmt.Errorf("sandboxset %s/%s has neither spec.templateRef nor spec.template", sbs.Namespace, sbs.Name)
	}

	return &agentsv1alpha1.SandboxTemplateSpec{
		Template:             sbs.Spec.Template.DeepCopy(),
		VolumeClaimTemplates: sbs.Spec.VolumeClaimTemplates,
		PersistentContents:   sbs.Spec.PersistentContents,
		Runtimes:             sbs.Spec.Runtimes,
	}, nil
}

// computeRevisionHash computes a stable FNV-32 hash from a SandboxTemplateSpec.
// The result is used for status.UpdateRevision and SandboxTemplate naming.
// Because json.Marshal produces deterministic output for Go structs (fields are
// serialised in declaration order), the same spec always yields the same hash.
func computeRevisionHash(spec *agentsv1alpha1.SandboxTemplateSpec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	hf := fnv.New32()
	hf.Write(data)
	return rand.SafeEncodeString(fmt.Sprint(hf.Sum32())), nil
}
