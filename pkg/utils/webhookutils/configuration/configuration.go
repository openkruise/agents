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
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	admissionregistrationv1defaults "k8s.io/kubernetes/pkg/apis/admissionregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/openkruise/agents/pkg/utils/webhookutils"
)

const (
	ValidatingWebhookConfigurationName = "sandbox-controller-validating-webhook-configuration"
	MutatingWebhookConfigurationName   = "sandbox-controller-mutating-webhook-configuration"

	webhookTemplateAnnotation              = "template"
	webhookDesiredTemplateAnnotation       = "agents.kruise.io/webhook-template"
	webhookTemplateRevisionAnnotation      = "agents.kruise.io/webhook-template-revision"
	webhookTemplateCacheRevisionAnnotation = "agents.kruise.io/webhook-template-cache-revision"
)

// Ensure ensures the webhook configurations are up to date.
func Ensure(kubeClient clientset.Interface, handlers map[string]admission.Handler, caBundle []byte) error {
	mutatingConfig, err := kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.TODO(), MutatingWebhookConfigurationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("not found MutatingWebhookConfiguration %s", MutatingWebhookConfigurationName)
	}
	validatingConfig, err := kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), ValidatingWebhookConfigurationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("not found ValidatingWebhookConfiguration %s", ValidatingWebhookConfigurationName)
	}
	oldMutatingConfig := mutatingConfig.DeepCopy()
	oldValidatingConfig := validatingConfig.DeepCopy()

	mutatingTemplate, err := parseMutatingTemplate(mutatingConfig)
	if err != nil {
		return err
	}
	validatingTemplate, err := parseValidatingTemplate(validatingConfig)
	if err != nil {
		return err
	}

	var mutatingWHs []admissionregistrationv1.MutatingWebhook
	for i := range mutatingTemplate {
		wh := &mutatingTemplate[i]
		wh.ClientConfig.CABundle = caBundle
		path, err := getPath(&wh.ClientConfig)
		if err != nil {
			return err
		}
		if _, ok := handlers[path]; !ok {
			klog.Warningf("Ignore webhook for %s in configuration", path)
			continue
		}
		if wh.ClientConfig.Service != nil {
			wh.ClientConfig.Service.Namespace = webhookutils.GetNamespace()
			wh.ClientConfig.Service.Name = webhookutils.GetServiceName()
		}
		if host := webhookutils.GetHost(); len(host) > 0 && wh.ClientConfig.Service != nil {
			convertClientConfig(&wh.ClientConfig, host, webhookutils.GetPort())
		}
		mutatingWHs = append(mutatingWHs, *wh)
	}
	mutatingConfig.Webhooks = mutatingWHs

	var validatingWHs []admissionregistrationv1.ValidatingWebhook
	for i := range validatingTemplate {
		wh := &validatingTemplate[i]
		wh.ClientConfig.CABundle = caBundle
		path, err := getPath(&wh.ClientConfig)
		if err != nil {
			return err
		}
		if _, ok := handlers[path]; !ok {
			klog.Warningf("Ignore webhook for %s in configuration", path)
			continue
		}
		if wh.ClientConfig.Service != nil {
			wh.ClientConfig.Service.Namespace = webhookutils.GetNamespace()
			wh.ClientConfig.Service.Name = webhookutils.GetServiceName()
		}
		if host := webhookutils.GetHost(); len(host) > 0 && wh.ClientConfig.Service != nil {
			convertClientConfig(&wh.ClientConfig, host, webhookutils.GetPort())
		}
		validatingWHs = append(validatingWHs, *wh)
	}
	validatingConfig.Webhooks = validatingWHs

	if !reflect.DeepEqual(mutatingConfig, oldMutatingConfig) {
		patch, err := createConfigurationPatch(oldMutatingConfig, mutatingConfig, admissionregistrationv1.MutatingWebhookConfiguration{})
		if err != nil {
			return fmt.Errorf("failed to create patch for %s: %w", MutatingWebhookConfigurationName, err)
		}
		if _, err := kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Patch(context.TODO(), mutatingConfig.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: "sandbox-controller"}); err != nil {
			return fmt.Errorf("failed to patch %s: %w", MutatingWebhookConfigurationName, err)
		}
	}

	if !reflect.DeepEqual(validatingConfig, oldValidatingConfig) {
		patch, err := createConfigurationPatch(oldValidatingConfig, validatingConfig, admissionregistrationv1.ValidatingWebhookConfiguration{})
		if err != nil {
			return fmt.Errorf("failed to create patch for %s: %w", ValidatingWebhookConfigurationName, err)
		}
		if _, err := kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Patch(context.TODO(), validatingConfig.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: "sandbox-controller"}); err != nil {
			return fmt.Errorf("failed to patch %s: %w", ValidatingWebhookConfigurationName, err)
		}
	}

	return nil
}

func createConfigurationPatch(oldConfig metav1.Object, newConfig, dataStruct any) ([]byte, error) {
	oldData, err := json.Marshal(oldConfig)
	if err != nil {
		return nil, err
	}
	newData, err := json.Marshal(newConfig)
	if err != nil {
		return nil, err
	}
	patch, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, dataStruct)
	if err != nil {
		return nil, err
	}

	var patchData map[string]any
	if err := json.Unmarshal(patch, &patchData); err != nil {
		return nil, err
	}
	metadata, ok := patchData["metadata"].(map[string]any)
	if !ok {
		metadata = make(map[string]any, 1)
		patchData["metadata"] = metadata
	}
	metadata["resourceVersion"] = oldConfig.GetResourceVersion()
	return json.Marshal(patchData)
}

func getPath(clientConfig *admissionregistrationv1.WebhookClientConfig) (string, error) {
	if clientConfig.Service != nil {
		if clientConfig.Service.Path == nil {
			return "", fmt.Errorf("webhook service path is required")
		}
		return *clientConfig.Service.Path, nil
	} else if clientConfig.URL != nil {
		u, err := url.Parse(*clientConfig.URL)
		if err != nil {
			return "", err
		}
		return u.Path, nil
	}
	return "", fmt.Errorf("invalid clientConfig: %+v", clientConfig)
}

func convertClientConfig(clientConfig *admissionregistrationv1.WebhookClientConfig, host string, port int) {
	url := fmt.Sprintf("https://%s:%d%s", host, port, *clientConfig.Service.Path)
	clientConfig.URL = &url
	clientConfig.Service = nil
}

func parseValidatingTemplate(validatingConfig *admissionregistrationv1.ValidatingWebhookConfiguration) ([]admissionregistrationv1.ValidatingWebhook, error) {
	return parseTemplate(&validatingConfig.Annotations, validatingConfig.Webhooks, defaultValidatingWebhook)
}

func parseMutatingTemplate(mutatingConfig *admissionregistrationv1.MutatingWebhookConfiguration) ([]admissionregistrationv1.MutatingWebhook, error) {
	return parseTemplate(&mutatingConfig.Annotations, mutatingConfig.Webhooks, defaultMutatingWebhook)
}

func defaultValidatingWebhook(webhook *admissionregistrationv1.ValidatingWebhook) {
	admissionregistrationv1defaults.SetDefaults_ValidatingWebhook(webhook)
	if webhook.ClientConfig.Service != nil {
		admissionregistrationv1defaults.SetDefaults_ServiceReference(webhook.ClientConfig.Service)
	}
	for i := range webhook.Rules {
		admissionregistrationv1defaults.SetDefaults_Rule(&webhook.Rules[i].Rule)
	}
}

func defaultMutatingWebhook(webhook *admissionregistrationv1.MutatingWebhook) {
	admissionregistrationv1defaults.SetDefaults_MutatingWebhook(webhook)
	if webhook.ClientConfig.Service != nil {
		admissionregistrationv1defaults.SetDefaults_ServiceReference(webhook.ClientConfig.Service)
	}
	for i := range webhook.Rules {
		admissionregistrationv1defaults.SetDefaults_Rule(&webhook.Rules[i].Rule)
	}
}

func parseTemplate[T any](annotations *map[string]string, webhooks []T, defaultWebhook func(*T)) ([]T, error) {
	currentAnnotations := *annotations
	desiredTemplate, hasDesiredTemplate := currentAnnotations[webhookDesiredTemplateAnnotation]
	revision, hasRevision := currentAnnotations[webhookTemplateRevisionAnnotation]
	if hasDesiredTemplate || hasRevision {
		if !hasDesiredTemplate || len(desiredTemplate) == 0 || !hasRevision || len(revision) == 0 {
			return nil, fmt.Errorf("webhook desired template and revision must both be set")
		}
		expectedRevision := fmt.Sprintf("%x", sha256.Sum256([]byte(desiredTemplate)))
		if revision != expectedRevision {
			return nil, fmt.Errorf("webhook template revision %q does not match desired template hash %q", revision, expectedRevision)
		}

		var desiredWebhooks []T
		if err := json.Unmarshal([]byte(desiredTemplate), &desiredWebhooks); err != nil {
			return nil, err
		}
		if len(desiredWebhooks) == 0 {
			return nil, fmt.Errorf("webhook desired template is empty")
		}
		for i := range desiredWebhooks {
			defaultWebhook(&desiredWebhooks[i])
		}
		templateBytes, err := json.Marshal(desiredWebhooks)
		if err != nil {
			return nil, err
		}
		currentAnnotations[webhookTemplateAnnotation] = string(templateBytes)
		currentAnnotations[webhookTemplateCacheRevisionAnnotation] = revision
		return desiredWebhooks, nil
	}

	template := currentAnnotations[webhookTemplateAnnotation]
	cacheRevision := currentAnnotations[webhookTemplateCacheRevisionAnnotation]
	if len(template) > 0 && revision == cacheRevision {
		var cachedWebhooks []T
		if err := json.Unmarshal([]byte(template), &cachedWebhooks); err != nil {
			return nil, err
		}
		if len(cachedWebhooks) == 0 {
			return nil, fmt.Errorf("webhook template cache is empty")
		}
		for i := range cachedWebhooks {
			defaultWebhook(&cachedWebhooks[i])
		}
		templateBytes, err := json.Marshal(cachedWebhooks)
		if err != nil {
			return nil, err
		}
		currentAnnotations[webhookTemplateAnnotation] = string(templateBytes)
		return cachedWebhooks, nil
	}
	if len(webhooks) == 0 {
		return nil, fmt.Errorf("webhook configuration is empty")
	}

	for i := range webhooks {
		defaultWebhook(&webhooks[i])
	}
	templateBytes, err := json.Marshal(webhooks)
	if err != nil {
		return nil, err
	}
	if currentAnnotations == nil {
		currentAnnotations = make(map[string]string, 2)
		*annotations = currentAnnotations
	}
	currentAnnotations[webhookTemplateAnnotation] = string(templateBytes)
	delete(currentAnnotations, webhookTemplateCacheRevisionAnnotation)
	return webhooks, nil
}
