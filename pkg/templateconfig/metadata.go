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

package templateconfig

import (
	"maps"
	"strings"

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const maxBusinessAnnotationSize = 64 * 1024

// Metadata holds user classification data, independent from PodTemplate metadata.
// It is projected onto native ObjectMeta, never persisted as a spec field.
type Metadata struct {
	Labels      map[string]string
	Annotations map[string]string
}

func (m Metadata) Clone() Metadata {
	return Metadata{Labels: maps.Clone(m.Labels), Annotations: maps.Clone(m.Annotations)}
}

// ReservedMetadataKey includes subdomains used by identity and revision hashes.
// Standard application labels such as app.kubernetes.io/name remain available.
func ReservedMetadataKey(key string) bool {
	domain, _, qualified := strings.Cut(key, "/")
	domain = strings.ToLower(domain)
	return qualified && (domain == "agents.kruise.io" || strings.HasSuffix(domain, ".agents.kruise.io"))
}

// UserMetadata returns an independent business-only projection. Controller and
// admission supplied keys in our reserved domains must not leak or propagate.
func UserMetadata(labels, annotations map[string]string) Metadata {
	filter := func(src map[string]string) map[string]string {
		var dst map[string]string
		for k, v := range src {
			if ReservedMetadataKey(k) {
				continue
			}
			if dst == nil {
				dst = make(map[string]string)
			}
			dst[k] = v
		}
		return dst
	}
	return Metadata{Labels: filter(labels), Annotations: filter(annotations)}
}

// ValidateMetadata is read-free and applies only to the custom root input, not
// to existing native resources or PodTemplate metadata.
func ValidateMetadata(m Metadata, path *field.Path) field.ErrorList {
	errs := metav1validation.ValidateLabels(m.Labels, path.Child("labels"))
	errs = append(errs, apivalidation.ValidateAnnotations(m.Annotations, path.Child("annotations"))...)
	annotationSize := 0
	for key, value := range m.Annotations {
		annotationSize += len(key) + len(value)
	}
	if annotationSize > maxBusinessAnnotationSize {
		errs = append(errs, field.TooLong(path.Child("annotations"), "", maxBusinessAnnotationSize))
	}
	for _, group := range []struct {
		name   string
		values map[string]string
	}{{"labels", m.Labels}, {"annotations", m.Annotations}} {
		for key := range group.values {
			if ReservedMetadataKey(key) {
				errs = append(errs, field.Forbidden(path.Child(group.name).Key(key), "reserved for Kruise Agents"))
			}
		}
	}
	return errs
}
