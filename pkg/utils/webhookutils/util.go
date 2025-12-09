package webhookutils

import (
	"os"
	"strconv"

	"k8s.io/klog/v2"
)

func GetHost() string {
	return os.Getenv("WEBHOOK_HOST")
}

func GetNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); len(ns) > 0 {
		return ns
	}
	return "sandbox-system"
}

func GetSecretName() string {
	if name := os.Getenv("SECRET_NAME"); len(name) > 0 {
		return name
	}
	return "sandbox-controller-webhook-certs"
}

func GetServiceName() string {
	if name := os.Getenv("SERVICE_NAME"); len(name) > 0 {
		return name
	}
	return "sandbox-controller-manager-webhook-service"
}

func GetPort() int {
	// svc port
	port := 443
	if p := os.Getenv("WEBHOOK_PORT"); len(p) > 0 {
		if p, err := strconv.ParseInt(p, 10, 32); err == nil {
			port = int(p)
		} else {
			klog.Fatalf("failed to convert WEBHOOK_PORT=%v in env: %v", p, err)
		}
	}
	return port
}

func GetCertDir() string {
	if p := os.Getenv("WEBHOOK_CERT_DIR"); len(p) > 0 {
		return p
	}
	return "/home/nonroot/sandbox-controller-webhook-certs"
}
