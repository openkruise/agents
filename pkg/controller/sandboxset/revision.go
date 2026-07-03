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
	"strconv"

	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	hf.Write(data) // #nosec G104 -- hash.Write never returns error
	return rand.SafeEncodeString(fmt.Sprint(hf.Sum32())), nil
}

// computeLegacyRevisionHash replicates the release-v0.3 hash algorithm so that
// existing sandboxes carrying the old-format hash are not incorrectly flagged
// as outdated after an in-place controller upgrade.
//
// Legacy algorithm:
//  1. JSON-marshal the PodTemplateSpec (inline or from referenced SandboxTemplate)
//  2. Unmarshal to map[string]interface{} and inject "$patch": "replace"
//  3. Wrap as {"spec": {"template": <map>}}
//  4. JSON-marshal the wrapper and FNV-32 hash the bytes
//
// The legacy hash only considers spec.template; it does NOT include
// VolumeClaimTemplates, PersistentContents, or Runtimes.
func (r *Reconciler) computeLegacyRevisionHash(_ context.Context, sbs *agentsv1alpha1.SandboxSet) (string, error) {
	// Legacy SandboxSets only use inline templates; templateRef was introduced
	// after the hash algorithm change, so no backward compat is needed for it.
	if sbs.Spec.TemplateRef != nil || sbs.Spec.Template == nil {
		return "", nil
	}
	specCopy := make(map[string]interface{})
	str, err := runtime.Encode(r.Codec, sbs)
	if err != nil {
		return "", err
	}
	var raw map[string]interface{}
	if err = json.Unmarshal(str, &raw); err != nil {
		return "", err
	}
	if spec, ok := raw["spec"].(map[string]interface{}); ok {
		if template, ok := spec["template"].(map[string]interface{}); ok {
			template["$patch"] = "replace"
			specCopy["template"] = template
		}
	}
	patch, err := json.Marshal(map[string]interface{}{"spec": specCopy})
	if err != nil {
		return "", err
	}
	// When the SandboxSet uses spec.templateRef, spec.template is nil. The
	// revision labels on ControllerRevision are only informational here
	// (the hash label is what the controller actually reads), so fall back
	// to an empty label set to avoid a nil dereference.
	var templateLabels map[string]string
	if sbs.Spec.Template != nil {
		templateLabels = sbs.Spec.Template.Labels
	}
	cr, err := NewControllerRevision(sbs,
		agentsv1alpha1.SandboxSetControllerKind,
		templateLabels,
		runtime.RawExtension{Raw: patch},
		0,
		nil)
	if err != nil {
		return "", err
	}
	return cr.Labels[ControllerRevisionHashLabel], nil
}

// NewControllerRevision returns a ControllerRevision with a ControllerRef pointing to parent and indicating that
// parent is of parentKind. The ControllerRevision has labels matching template labels, contains Data equal to data, and
// has a Revision equal to revision. The collisionCount is used when creating the name of the ControllerRevision
// so the name is likely unique. If the returned error is nil, the returned ControllerRevision is valid. If the
// returned error is not nil, the returned ControllerRevision is invalid for use.
func NewControllerRevision(parent metav1.Object,
	parentKind schema.GroupVersionKind,
	templateLabels map[string]string,
	data runtime.RawExtension,
	revision int64,
	collisionCount *int32) (*apps.ControllerRevision, error) {
	labelMap := make(map[string]string)
	for k, v := range templateLabels {
		labelMap[k] = v
	}
	cr := &apps.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Labels:          labelMap,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, parentKind)},
		},
		Data:     data,
		Revision: revision,
	}
	hash := HashControllerRevision(cr, collisionCount)
	cr.Labels[ControllerRevisionHashLabel] = hash
	return cr, nil
}

// ControllerRevisionHashLabel is the label used to indicate the hash value of a ControllerRevision's Data.
const ControllerRevisionHashLabel = "controller.kubernetes.io/hash"

// HashControllerRevision hashes the contents of revision's Data using FNV hashing. If probe is not nil, the byte value
// of probe is added written to the hash as well. The returned hash will be a safe encoded string to avoid bad words.
func HashControllerRevision(revision *apps.ControllerRevision, probe *int32) string {
	hf := fnv.New32()
	if len(revision.Data.Raw) > 0 {
		hf.Write(revision.Data.Raw) // #nosec G104 -- hash.Write never returns error
	}
	if probe != nil {
		hf.Write([]byte(strconv.FormatInt(int64(*probe), 10))) // #nosec G104 -- hash.Write never returns error
	}
	return rand.SafeEncodeString(fmt.Sprint(hf.Sum32()))
}
