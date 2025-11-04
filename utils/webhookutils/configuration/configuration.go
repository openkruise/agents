package configuration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"

	"gitlab.alibaba-inc.com/serverlessinfra/agents/utils/webhookutils"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	ValidatingWebhookConfigurationName = "kruise-sandbox-validating-webhook-configuration"
)

func Ensure(kubeClient clientset.Interface, handlers map[string]admission.Handler, caBundle []byte) error {
	validatingConfig, err := kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), ValidatingWebhookConfigurationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("not found ValidatingWebhookConfiguration %s", ValidatingWebhookConfigurationName)
	}
	oldValidatingConfig := validatingConfig.DeepCopy()

	validatingTemplate, err := parseValidatingTemplate(validatingConfig)
	if err != nil {
		return err
	}

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

	if !reflect.DeepEqual(validatingConfig, oldValidatingConfig) {
		if _, err := kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(context.TODO(), validatingConfig, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update %s: %v", ValidatingWebhookConfigurationName, err)
		}
	}

	return nil
}

func getPath(clientConfig *admissionregistrationv1.WebhookClientConfig) (string, error) {
	if clientConfig.Service != nil {
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
	if templateStr := validatingConfig.Annotations["template"]; len(templateStr) > 0 {
		var validatingWHs []admissionregistrationv1.ValidatingWebhook
		if err := json.Unmarshal([]byte(templateStr), &validatingWHs); err != nil {
			return nil, err
		}
		return validatingWHs, nil
	}

	templateBytes, err := json.Marshal(validatingConfig.Webhooks)
	if err != nil {
		return nil, err
	}
	if validatingConfig.Annotations == nil {
		validatingConfig.Annotations = make(map[string]string, 1)
	}
	validatingConfig.Annotations["template"] = string(templateBytes)
	return validatingConfig.Webhooks, nil
}
