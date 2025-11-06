// Package e2b provides HTTP controllers for handling E2B sandbox requests.
package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/controllers/e2b/models"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/utils"
	"github.com/openkruise/agents/pkg/sandbox-manager/web"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

var (
	Namespace = "default"
)

var (
	browserWebSocketReplacer = regexp.MustCompile(`^ws://[^/]+`)
)

func (sc *Controller) initEnvd(ctx context.Context, sbx infra.Sandbox, envVars models.EnvVars) error {
	start := time.Now()
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetName(), "envVars", envVars)
	initBody, err := json.Marshal(map[string]any{
		"envVars": envVars,
	})
	if err != nil {
		log.Error(err, "failed to marshal initBody")
		return err
	}
	request, err := http.NewRequest(http.MethodPost, "/init", bytes.NewBuffer(initBody))
	if err != nil {
		log.Error(err, "failed to create init request")
		return err
	}
	_, err = sbx.Request(request, "/init", models.EnvdPort)
	if err != nil {
		log.Error(err, "failed to init envd")
		return err
	}
	log.Info("envd inited", "cost", time.Since(start))
	return nil
}

func (sc *Controller) createServiceAndIngressForPod(ctx context.Context, sbx infra.Sandbox) error {
	var ports []corev1.ServicePort
	var hosts []string
	var rules []networkingv1.IngressRule
	pool, ok := sc.manager.GetInfra().GetPoolByObject(sbx)
	if !ok {
		return fmt.Errorf("pool not found for sandbox %s", sbx.GetName())
	}
	for _, container := range pool.GetTemplate().Spec.Template.Spec.Containers {
		for _, port := range container.Ports {
			ports = append(ports, corev1.ServicePort{
				Name:       port.Name,
				Port:       port.ContainerPort,
				TargetPort: intstr.FromInt32(port.ContainerPort),
			})
			hosts = append(hosts, utils.GetSandboxAddress(sbx.GetName(), sc.domain, port.ContainerPort))
			rules = append(rules, utils.SandboxIngressRule(sbx.GetName(), sc.domain, port.ContainerPort))
		}
	}
	sandboxLabels := map[string]string{
		consts.LabelSandboxPool: sbx.GetLabels()[consts.LabelSandboxPool],
		consts.LabelSandboxID:   sbx.GetName(),
	}
	// Define the Service spec
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sbx.GetName(),
			Namespace: sbx.GetNamespace(),
			Labels:    sandboxLabels,
		},
		Spec: corev1.ServiceSpec{
			Selector: sandboxLabels,
			Ports:    ports,
		},
	}

	// Define the Ingress spec
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sbx.GetName(),
			Namespace: sbx.GetNamespace(),
			Labels:    sandboxLabels,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To("mse"), // 本地开发只支持 MSE 减少折腾
			TLS: []networkingv1.IngressTLS{
				{
					Hosts:      hosts,
					SecretName: sc.tlsSecret,
				},
			},
			Rules: rules,
		},
	}
	ownerReference := []metav1.OwnerReference{{
		APIVersion:         "v1",
		Kind:               "Pod",
		Name:               sbx.GetName(),
		UID:                sbx.GetUID(),
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
	svc.OwnerReferences = ownerReference
	ing.OwnerReferences = ownerReference

	// Create the Service in Kubernetes
	_, err := sc.client.CoreV1().Services(sbx.GetNamespace()).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create sandbox service: %w", err)
	}

	// Create the Ingress in Kubernetes
	createdIngress, err := sc.client.NetworkingV1().Ingresses(sbx.GetNamespace()).Create(ctx, ing, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create sandbox ingress: %w", err)
	}

	start := time.Now()
	for {
		if time.Since(start) > 5*time.Minute {
			return fmt.Errorf("ingress %s did not become ready within timeout", createdIngress.Name)
		}
		gotIngress, err := sc.client.NetworkingV1().Ingresses(sbx.GetNamespace()).Get(ctx, createdIngress.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		// Check if the gotIngress has IP/Hostname assigned
		if len(gotIngress.Status.LoadBalancer.Ingress) > 0 {
			break
		}
	}
	klog.InfoS("gateway ready for sandbox", "sandbox", klog.KObj(sbx))
	return nil
}

func (sc *Controller) pauseAndResumeSandbox(r *http.Request, pause bool) (web.ApiResponse[struct{}], *web.ApiError) {
	id := r.PathValue("sandboxID")
	ctx := r.Context()
	log := klog.FromContext(ctx).WithValues("sandboxID", id)
	sbx, err := sc.manager.GetClaimedSandbox(id)
	if err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Sandbox %s not found", id),
		}
	}
	if pause {
		if state := sbx.GetState(); state != consts.SandboxStateRunning {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("Sandbox %s is not running", id),
			}
		}
		if err = sbx.Pause(ctx); err != nil {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Message: fmt.Sprintf("Failed to pause sandbox: %v", err),
			}
		}
		log.Info("sandbox paused")
	} else {
		if state := sbx.GetState(); state != consts.SandboxStatePaused {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("Sandbox %s is not paused", id),
			}
		}
		if err = sbx.Resume(ctx); err != nil {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Message: fmt.Sprintf("Failed to resume sandbox: %v", err),
			}
		}
		log.Info("sandbox resumed")
	}
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) convertToE2BSandbox(ctx context.Context, sbx infra.Sandbox) *models.Sandbox {
	log := klog.FromContext(ctx).V(DebugLevel).WithValues("sandbox", klog.KObj(sbx))

	sandbox := &models.Sandbox{
		SandboxID:   sbx.GetName(),
		TemplateID:  sbx.GetTemplate(),
		Domain:      sc.domain,
		EnvdVersion: "0.1.1",
		State:       sbx.GetState(),
	}
	route, ok := sc.manager.GetRoute(sbx.GetName())
	if ok {
		key, ok := sc.keys.LoadByID(route.Owner)
		if !ok {
			log.Info("skip convert sandbox route to e2b key: key for user not found")
		} else {
			sandbox.EnvdAccessToken = key.Key
		}
	} else {
		log.Info("skip convert sandbox route to e2b key: route for sandbox not found")
	}
	// Just for example
	sandbox.StartedAt = sbx.GetCreationTimestamp().Format(time.RFC3339)
	sandbox.EndAt = sbx.GetCreationTimestamp().Add(1000 * time.Hour).Format(time.RFC3339)
	resource := sbx.GetResource()
	sandbox.CPUCount = resource.CPUMilli / 1000
	sandbox.MemoryMB = resource.MemoryMB
	return sandbox
}
