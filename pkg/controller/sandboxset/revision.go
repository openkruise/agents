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

// Copied From Kubernetes

package sandboxset

import (
	"context"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"strconv"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getPatch serialises the subset of the SandboxSet spec that determines the
// template revision hash. Both spec.template (inline) and spec.templateRef are
// normalised to the same `spec.template` key so that they yield identical
// hashes whenever they describe the same pod template. This also guarantees
// that switching between inline template and templateRef (or between two
// templateRefs) only triggers a rolling update when the underlying pod
// template actually differs.
func (r *Reconciler) getPatch(ctx context.Context, set *agentsv1alpha1.SandboxSet) ([]byte, error) {
	specCopy := make(map[string]interface{})
	// spec.template and spec.templateRef are mutually exclusive; prefer
	// templateRef when set and fall back to the inline template otherwise.
	if set.Spec.TemplateRef != nil {
		tpl := &agentsv1alpha1.SandboxTemplate{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: set.Namespace,
			Name:      set.Spec.TemplateRef.Name,
		}, tpl); err != nil {
			return nil, fmt.Errorf("failed to resolve sandbox template %s/%s: %w",
				set.Namespace, set.Spec.TemplateRef.Name, err)
		}
		if tpl.Spec.Template != nil {
			templateRaw, err := json.Marshal(tpl.Spec.Template)
			if err != nil {
				return nil, err
			}
			var templateMap map[string]interface{}
			if err := json.Unmarshal(templateRaw, &templateMap); err != nil {
				return nil, err
			}
			templateMap["$patch"] = "replace"
			specCopy["template"] = templateMap
		}
	} else if set.Spec.Template != nil {
		str, err := runtime.Encode(r.Codec, set)
		if err != nil {
			return nil, err
		}
		var raw map[string]interface{}
		if err = json.Unmarshal(str, &raw); err != nil {
			return nil, err
		}
		if spec, ok := raw["spec"].(map[string]interface{}); ok {
			if template, ok := spec["template"].(map[string]interface{}); ok {
				template["$patch"] = "replace"
				specCopy["template"] = template
			}
		}
	}
	return json.Marshal(map[string]interface{}{"spec": specCopy})
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
	cr.Name = ControllerRevisionName(parent.GetName(), hash)
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
		hf.Write(revision.Data.Raw)
	}
	if revision.Data.Object != nil {
		DeepHashObject(hf, revision.Data.Object)
	}
	if probe != nil {
		hf.Write([]byte(strconv.FormatInt(int64(*probe), 10)))
	}
	return rand.SafeEncodeString(fmt.Sprint(hf.Sum32()))
}

// ControllerRevisionName returns the Name for a ControllerRevision in the form prefix-hash. If the length
// of prefix is greater than 223 bytes, it is truncated to allow for a name that is no larger than 253 bytes.
func ControllerRevisionName(prefix string, hash string) string {
	if len(prefix) > 223 {
		prefix = prefix[:223]
	}

	return fmt.Sprintf("%s-%s", prefix, hash)
}

// DeepHashObject writes specified object to hash using the spew library
// which follows pointers and prints actual values of the nested objects
// ensuring the hash does not change when a pointer changes.
func DeepHashObject(hasher hash.Hash, objectToWrite interface{}) {
	hasher.Reset()
	fmt.Fprintf(hasher, "%v", dump.ForHash(objectToWrite))
}
