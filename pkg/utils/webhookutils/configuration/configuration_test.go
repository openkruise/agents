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

package configuration

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

type templateWebhook struct {
	Name string `json:"name"`
}

func TestParseTemplate(t *testing.T) {
	current := []templateWebhook{{Name: "current"}}
	cached := []templateWebhook{{Name: "cached"}}
	desired := []templateWebhook{{Name: "desired"}}
	cachedBytes, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	desiredBytes, err := json.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	desiredRevision := templateRevision(string(desiredBytes))
	invalidDesired := "{"
	nullDesired := "null"

	tests := []struct {
		name                 string
		annotations          map[string]string
		want                 []templateWebhook
		wantStored           []templateWebhook
		wantCacheRevision    string
		wantCacheRevisionSet bool
		wantErr              bool
	}{
		{
			name: "restore chart-owned template over stale cache",
			annotations: map[string]string{
				webhookDesiredTemplateAnnotation:       string(desiredBytes),
				webhookTemplateRevisionAnnotation:      desiredRevision,
				webhookTemplateAnnotation:              string(cachedBytes),
				webhookTemplateCacheRevisionAnnotation: "old",
			},
			want:                 desired,
			wantStored:           desired,
			wantCacheRevision:    desiredRevision,
			wantCacheRevisionSet: true,
		},
		{
			name: "repair stale cache with matching revision",
			annotations: map[string]string{
				webhookDesiredTemplateAnnotation:       string(desiredBytes),
				webhookTemplateRevisionAnnotation:      desiredRevision,
				webhookTemplateAnnotation:              string(cachedBytes),
				webhookTemplateCacheRevisionAnnotation: desiredRevision,
			},
			want:                 desired,
			wantStored:           desired,
			wantCacheRevision:    desiredRevision,
			wantCacheRevisionSet: true,
		},
		{
			name: "reject mismatched desired template hash",
			annotations: map[string]string{
				webhookDesiredTemplateAnnotation:  string(desiredBytes),
				webhookTemplateRevisionAnnotation: "wrong",
			},
			wantErr: true,
		},
		{
			name: "reject missing desired template",
			annotations: map[string]string{
				webhookTemplateRevisionAnnotation: desiredRevision,
			},
			wantErr: true,
		},
		{
			name: "reject missing desired template revision",
			annotations: map[string]string{
				webhookDesiredTemplateAnnotation: string(desiredBytes),
			},
			wantErr: true,
		},
		{
			name: "reject invalid desired template",
			annotations: map[string]string{
				webhookDesiredTemplateAnnotation:  invalidDesired,
				webhookTemplateRevisionAnnotation: templateRevision(invalidDesired),
			},
			wantErr: true,
		},
		{
			name: "reject null desired template",
			annotations: map[string]string{
				webhookDesiredTemplateAnnotation:  nullDesired,
				webhookTemplateRevisionAnnotation: templateRevision(nullDesired),
			},
			wantErr: true,
		},
		{
			name:       "initialize missing cache",
			want:       current,
			wantStored: current,
		},
		{
			name: "use legacy cache",
			annotations: map[string]string{
				webhookTemplateAnnotation: string(cachedBytes),
			},
			want:       cached,
			wantStored: cached,
		},
		{
			name: "refresh removed chart source",
			annotations: map[string]string{
				webhookTemplateAnnotation:              string(cachedBytes),
				webhookTemplateCacheRevisionAnnotation: desiredRevision,
			},
			want:       current,
			wantStored: current,
		},
		{
			name: "reject invalid legacy cache",
			annotations: map[string]string{
				webhookTemplateAnnotation: "{",
			},
			wantErr: true,
		},
		{
			name: "reject empty legacy cache",
			annotations: map[string]string{
				webhookTemplateAnnotation: "[]",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := maps.Clone(tt.annotations)
			got, err := parseTemplate(&annotations, current, func(*templateWebhook) {})
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseTemplate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseTemplate() = %#v, want %#v", got, tt.want)
			}

			var stored []templateWebhook
			if err := json.Unmarshal([]byte(annotations[webhookTemplateAnnotation]), &stored); err != nil {
				t.Fatalf("unmarshal stored template: %v", err)
			}
			if !reflect.DeepEqual(stored, tt.wantStored) {
				t.Errorf("stored template = %#v, want %#v", stored, tt.wantStored)
			}
			cacheRevision, cacheRevisionSet := annotations[webhookTemplateCacheRevisionAnnotation]
			if cacheRevision != tt.wantCacheRevision || cacheRevisionSet != tt.wantCacheRevisionSet {
				t.Errorf("cached revision = %q, %v, want %q, %v", cacheRevision, cacheRevisionSet, tt.wantCacheRevision, tt.wantCacheRevisionSet)
			}
		})
	}
}

func TestParseMutatingTemplateAppliesAPIDefaults(t *testing.T) {
	path := "/test"
	desired := []admissionregistrationv1.MutatingWebhook{
		{
			Name: "test",
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{
					Name:      "service",
					Namespace: "namespace",
					Path:      &path,
				},
			},
			Rules: []admissionregistrationv1.RuleWithOperations{{}},
		},
	}
	desiredBytes, err := json.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	config := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			webhookDesiredTemplateAnnotation:  string(desiredBytes),
			webhookTemplateRevisionAnnotation: templateRevision(string(desiredBytes)),
		}},
		Webhooks: desired,
	}

	got, err := parseMutatingTemplate(config)
	if err != nil {
		t.Fatal(err)
	}
	webhook := got[0]
	if webhook.MatchPolicy == nil || webhook.NamespaceSelector == nil || webhook.ObjectSelector == nil || webhook.TimeoutSeconds == nil || webhook.ReinvocationPolicy == nil {
		t.Fatalf("webhook defaults were not applied: %#v", webhook)
	}
	if webhook.ClientConfig.Service.Port == nil || webhook.Rules[0].Scope == nil {
		t.Fatalf("nested webhook defaults were not applied: %#v", webhook)
	}

	cached := config.Annotations[webhookTemplateAnnotation]
	config.Webhooks = got
	if _, err := parseMutatingTemplate(config); err != nil {
		t.Fatal(err)
	}
	if config.Annotations[webhookTemplateAnnotation] != cached {
		t.Error("normalized webhook cache changed on the second reconciliation")
	}
}

func TestCreateConfigurationPatchPreservesChartOwnedAnnotations(t *testing.T) {
	oldConfig := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:            MutatingWebhookConfigurationName,
			ResourceVersion: "42",
			Annotations: map[string]string{
				webhookDesiredTemplateAnnotation:       "desired",
				webhookTemplateRevisionAnnotation:      "revision",
				webhookTemplateAnnotation:              "old-cache",
				webhookTemplateCacheRevisionAnnotation: "old-revision",
			},
		},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{Name: "webhook"}},
	}
	newConfig := oldConfig.DeepCopy()
	newConfig.Annotations[webhookTemplateAnnotation] = "new-cache"
	newConfig.Annotations[webhookTemplateCacheRevisionAnnotation] = "revision"
	newConfig.Webhooks[0].ClientConfig.CABundle = []byte("ca")

	patch, err := createConfigurationPatch(oldConfig, newConfig, admissionregistrationv1.MutatingWebhookConfiguration{})
	if err != nil {
		t.Fatal(err)
	}

	var patchData map[string]any
	if err := json.Unmarshal(patch, &patchData); err != nil {
		t.Fatal(err)
	}
	metadata := patchData["metadata"].(map[string]any)
	if metadata["resourceVersion"] != "42" {
		t.Errorf("resourceVersion = %v, want 42", metadata["resourceVersion"])
	}
	annotations := metadata["annotations"].(map[string]any)
	if _, ok := annotations[webhookDesiredTemplateAnnotation]; ok {
		t.Errorf("patch claims chart-owned annotation %q", webhookDesiredTemplateAnnotation)
	}
	if _, ok := annotations[webhookTemplateRevisionAnnotation]; ok {
		t.Errorf("patch claims chart-owned annotation %q", webhookTemplateRevisionAnnotation)
	}

	oldData, err := json.Marshal(oldConfig)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := strategicpatch.StrategicMergePatch(oldData, patch, admissionregistrationv1.MutatingWebhookConfiguration{})
	if err != nil {
		t.Fatal(err)
	}
	var got admissionregistrationv1.MutatingWebhookConfiguration
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&got, newConfig) {
		t.Errorf("patched configuration = %#v, want %#v", &got, newConfig)
	}
}

func TestGetPathRejectsMissingServicePath(t *testing.T) {
	_, err := getPath(&admissionregistrationv1.WebhookClientConfig{
		Service: &admissionregistrationv1.ServiceReference{},
	})
	if err == nil {
		t.Fatal("getPath() returned no error for a missing service path")
	}
}

func templateRevision(template string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(template)))
}
