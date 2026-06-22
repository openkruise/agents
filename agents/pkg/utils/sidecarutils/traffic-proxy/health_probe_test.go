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

package trafficproxy

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestFindContainerFromPod(t *testing.T) {
	tests := []struct {
		name       string
		pod        *corev1.Pod
		lookup     string
		expectNil  bool
		expectName string
	}{
		{
			name: "find in containers",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers:     []corev1.Container{{Name: "app"}, {Name: "sidecar"}},
					InitContainers: []corev1.Container{{Name: "init"}},
				},
			},
			lookup:     "app",
			expectNil:  false,
			expectName: "app",
		},
		{
			name: "find in init containers",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers:     []corev1.Container{{Name: "app"}},
					InitContainers: []corev1.Container{{Name: "init"}},
				},
			},
			lookup:     "init",
			expectNil:  false,
			expectName: "init",
		},
		{
			name: "not found",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app"}},
				},
			},
			lookup:    "missing",
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := findContainerFromPod(tt.lookup, tt.pod)
			if tt.expectNil && c != nil {
				t.Errorf("expected nil, got %q", c.Name)
			}
			if !tt.expectNil && (c == nil || c.Name != tt.expectName) {
				t.Errorf("expected %q, got %v", tt.expectName, c)
			}
		})
	}
}

func TestFindSidecar(t *testing.T) {
	t.Run("sidecar in containers", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: ProxyContainerName}},
			},
		}
		c := findSidecar(pod, ProxyContainerName)
		if c == nil || c.Name != ProxyContainerName {
			t.Errorf("expected to find %q, got %v", ProxyContainerName, c)
		}
	})

	t.Run("sidecar in init containers", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: ProxyContainerName}},
			},
		}
		c := findSidecar(pod, ProxyContainerName)
		if c == nil || c.Name != ProxyContainerName {
			t.Errorf("expected to find %q, got %v", ProxyContainerName, c)
		}
	})

	t.Run("no sidecar", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app"}},
			},
		}
		if c := findSidecar(pod, ProxyContainerName); c != nil {
			t.Errorf("expected nil, got %q", c.Name)
		}
	})
}

func TestRewriteHTTPGetAction(t *testing.T) {
	tests := []struct {
		name         string
		action       *corev1.HTTPGetAction
		url          string
		port         int
		expectPath   string
		expectPort   int32
		expectScheme corev1.URIScheme
	}{
		{
			name:         "HTTP scheme preserved",
			action:       &corev1.HTTPGetAction{Path: "/old", Port: intstr.FromInt32(8080), Scheme: corev1.URISchemeHTTP},
			url:          "/new",
			port:         15020,
			expectPath:   "/new",
			expectPort:   15020,
			expectScheme: corev1.URISchemeHTTP,
		},
		{
			name:         "HTTPS scheme rewritten to HTTP",
			action:       &corev1.HTTPGetAction{Path: "/old", Port: intstr.FromInt32(8080), Scheme: corev1.URISchemeHTTPS},
			url:          "/new",
			port:         15020,
			expectPath:   "/new",
			expectPort:   15020,
			expectScheme: corev1.URISchemeHTTP,
		},
		{
			name:         "empty scheme defaults to HTTP",
			action:       &corev1.HTTPGetAction{Path: "/old", Port: intstr.FromInt32(8080)},
			url:          "/health",
			port:         15000,
			expectPath:   "/health",
			expectPort:   15000,
			expectScheme: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rewriteHTTPGetAction(tt.action, tt.url, tt.port)
			if tt.action.Path != tt.expectPath {
				t.Errorf("path: expected %q, got %q", tt.expectPath, tt.action.Path)
			}
			if tt.action.Port.IntVal != tt.expectPort {
				t.Errorf("port: expected %d, got %d", tt.expectPort, tt.action.Port.IntVal)
			}
			if tt.action.Scheme != tt.expectScheme {
				t.Errorf("scheme: expected %q, got %q", tt.expectScheme, tt.action.Scheme)
			}
		})
	}
}

func TestConvertAppProber(t *testing.T) {
	t.Run("nil probe returns nil", func(t *testing.T) {
		if result := convertAppProber(nil, "/readyz", 15020); result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})

	t.Run("HTTPGet probe converted", func(t *testing.T) {
		probe := &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080)},
			},
		}
		result := convertAppProber(probe, "/readyz", 15020)
		if result == nil || result.HTTPGet == nil {
			t.Fatal("expected HTTPGet probe to be converted")
		}
		if result.HTTPGet.Path != "/readyz" {
			t.Errorf("expected path /readyz, got %s", result.HTTPGet.Path)
		}
		if result.HTTPGet.Port.IntVal != 15020 {
			t.Errorf("expected port 15020, got %d", result.HTTPGet.Port.IntVal)
		}
	})

	t.Run("TCPSocket probe converted to HTTP", func(t *testing.T) {
		probe := &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)},
			},
		}
		result := convertAppProber(probe, "/readyz", 15020)
		if result == nil {
			t.Fatal("expected TCP probe to be converted")
		}
		if result.HTTPGet == nil {
			t.Error("expected TCP to be converted to HTTPGet")
		}
		if result.TCPSocket != nil {
			t.Error("expected TCPSocket to be nil after conversion")
		}
	})

	t.Run("GRPC probe converted to HTTP", func(t *testing.T) {
		probe := &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				GRPC: &corev1.GRPCAction{Port: 50051},
			},
		}
		result := convertAppProber(probe, "/readyz", 15020)
		if result == nil {
			t.Fatal("expected GRPC probe to be converted")
		}
		if result.HTTPGet == nil {
			t.Error("expected GRPC to be converted to HTTPGet")
		}
		if result.GRPC != nil {
			t.Error("expected GRPC to be nil after conversion")
		}
	})

	t.Run("unsupported probe type returns nil", func(t *testing.T) {
		// A probe with no handler fields
		probe := &corev1.Probe{}
		result := convertAppProber(probe, "/readyz", 15020)
		if result != nil {
			t.Errorf("expected nil for unsupported probe, got %v", result)
		}
	})
}

func TestConvertAppLifecycleHandler(t *testing.T) {
	t.Run("nil handler returns nil", func(t *testing.T) {
		if result := convertAppLifecycleHandler(nil, "/prestopz", 15020); result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})

	t.Run("HTTPGet handler converted", func(t *testing.T) {
		h := &corev1.LifecycleHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/shutdown", Port: intstr.FromInt32(8080)},
		}
		result := convertAppLifecycleHandler(h, "/prestopz", 15020)
		if result == nil || result.HTTPGet == nil {
			t.Fatal("expected HTTPGet to be converted")
		}
		if result.HTTPGet.Path != "/prestopz" {
			t.Errorf("expected path /prestopz, got %s", result.HTTPGet.Path)
		}
	})

	t.Run("TCPSocket handler converted to HTTP", func(t *testing.T) {
		h := &corev1.LifecycleHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)},
		}
		result := convertAppLifecycleHandler(h, "/prestopz", 15020)
		if result == nil || result.HTTPGet == nil {
			t.Fatal("expected TCPSocket to be converted to HTTPGet")
		}
		if result.TCPSocket != nil {
			t.Error("expected TCPSocket to be nil after conversion")
		}
	})

	t.Run("unsupported handler returns nil", func(t *testing.T) {
		h := &corev1.LifecycleHandler{}
		result := convertAppLifecycleHandler(h, "/prestopz", 15020)
		if result != nil {
			t.Errorf("expected nil for unsupported handler, got %v", result)
		}
	})
}

func TestFormatProberURL(t *testing.T) {
	readyz, livez, startupz, prestopz, poststartz := formatProberURL("my-app")

	expected := []struct {
		name string
		got  string
	}{
		{"readyz", readyz},
		{"livez", livez},
		{"startupz", startupz},
		{"prestopz", prestopz},
		{"poststartz", poststartz},
	}

	for _, e := range expected {
		t.Run(e.name, func(t *testing.T) {
			expectedPath := "/app-health/my-app/" + e.name
			if e.name == "prestopz" || e.name == "poststartz" {
				expectedPath = "/app-lifecycle/my-app/" + e.name
			}
			if e.got != expectedPath {
				t.Errorf("expected %q, got %q", expectedPath, e.got)
			}
		})
	}
}

func TestDumpAppProbers(t *testing.T) {
	t.Run("pod with no probes returns empty string", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app"}},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		if result != "" {
			t.Errorf("expected empty string, got %q", result)
		}
	})

	t.Run("pod with HTTPGet readiness probe", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8080)},
							},
						},
					},
				},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		if result == "" {
			t.Fatal("expected non-empty probers JSON")
		}

		var probers KubeAppProbers
		if err := json.Unmarshal([]byte(result), &probers); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}

		readyzKey := "/app-health/app/readyz"
		if _, ok := probers[readyzKey]; !ok {
			t.Errorf("expected prober key %q, got keys: %v", readyzKey, probers)
		}
	})

	t.Run("named port resolved to integer", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: 8080},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromString("http")},
							},
						},
					},
				},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		if result == "" {
			t.Fatal("expected non-empty probers JSON")
		}

		var probers KubeAppProbers
		if err := json.Unmarshal([]byte(result), &probers); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}

		readyzKey := "/app-health/app/readyz"
		p, ok := probers[readyzKey]
		if !ok {
			t.Fatalf("expected prober key %q", readyzKey)
		}
		if p.HTTPGet.Port.IntVal != 8080 {
			t.Errorf("expected named port resolved to 8080, got %d", p.HTTPGet.Port.IntVal)
		}
	})

	t.Run("named port not found returns empty for that probe", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromString("unknown-port")},
							},
						},
					},
				},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		if result != "" {
			t.Errorf("expected empty string for unresolved named port, got %q", result)
		}
	})

	t.Run("skip istio-proxy container", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: ProxyContainerName,
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(15020)},
							},
						},
					},
				},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		if result != "" {
			t.Errorf("expected empty string (proxy container skipped), got %q", result)
		}
	})

	t.Run("liveness and startup probes included", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/live", Port: intstr.FromInt32(8080)},
							},
						},
						StartupProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/startup", Port: intstr.FromInt32(8080)},
							},
						},
					},
				},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		if result == "" {
			t.Fatal("expected non-empty probers JSON")
		}

		var probers KubeAppProbers
		if err := json.Unmarshal([]byte(result), &probers); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}

		for _, key := range []string{"/app-health/app/livez", "/app-health/app/startupz"} {
			if _, ok := probers[key]; !ok {
				t.Errorf("expected prober key %q, got keys: %v", key, probers)
			}
		}
	})

	t.Run("lifecycle preStop and postStart included", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						Lifecycle: &corev1.Lifecycle{
							PreStop: &corev1.LifecycleHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/prestop", Port: intstr.FromInt32(8080)},
							},
							PostStart: &corev1.LifecycleHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/poststart", Port: intstr.FromInt32(8080)},
							},
						},
					},
				},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		if result == "" {
			t.Fatal("expected non-empty probers JSON")
		}

		var probers KubeAppProbers
		if err := json.Unmarshal([]byte(result), &probers); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}

		for _, key := range []string{"/app-lifecycle/app/prestopz", "/app-lifecycle/app/poststartz"} {
			if _, ok := probers[key]; !ok {
				t.Errorf("expected prober key %q, got keys: %v", key, probers)
			}
		}
	})

	t.Run("TCP probe serialized as TCPSocket", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)},
							},
						},
					},
				},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		var probers KubeAppProbers
		if err := json.Unmarshal([]byte(result), &probers); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		p := probers["/app-health/app/readyz"]
		// DumpAppProbers uses kubeProbeToInternalProber which preserves original type
		if p.TCPSocket == nil {
			t.Error("expected TCP probe to be serialized as TCPSocket in prober")
		}
	})

	t.Run("GRPC probe serialized as GRPC", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								GRPC: &corev1.GRPCAction{Port: 50051},
							},
						},
					},
				},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		var probers KubeAppProbers
		if err := json.Unmarshal([]byte(result), &probers); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		p := probers["/app-health/app/readyz"]
		// DumpAppProbers preserves GRPC type
		if p.GRPC == nil {
			t.Error("expected GRPC probe to be serialized as GRPC in prober")
		}
	})

	t.Run("init containers included", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{
						Name: "init-app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(9090)},
							},
						},
					},
				},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		var probers KubeAppProbers
		if err := json.Unmarshal([]byte(result), &probers); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if _, ok := probers["/app-health/init-app/readyz"]; !ok {
			t.Errorf("expected init container prober key, got keys: %v", probers)
		}
	})

	t.Run("already rewritten port skipped", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(15020)},
							},
						},
					},
				},
			},
		}
		result := dumpAppProbers(pod, 15020, ProxyContainerName)
		// Port 15020 is the targetPort, so it's considered already rewritten
		if result != "" {
			t.Errorf("expected empty string for already-rewritten port, got %q", result)
		}
	})
}

func TestPatchRewriteProbe(t *testing.T) {
	t.Run("HTTPGet probes rewritten", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080), Scheme: corev1.URISchemeHTTPS},
							},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/live", Port: intstr.FromInt32(8080)},
							},
						},
					},
				},
			},
		}

		patchRewriteProbe(pod, 15020, ProxyContainerName)

		c := pod.Spec.Containers[0]
		if c.ReadinessProbe.HTTPGet.Path != "/app-health/app/readyz" {
			t.Errorf("readiness path: expected /app-health/app/readyz, got %s", c.ReadinessProbe.HTTPGet.Path)
		}
		if c.ReadinessProbe.HTTPGet.Port.IntVal != 15020 {
			t.Errorf("readiness port: expected 15020, got %d", c.ReadinessProbe.HTTPGet.Port.IntVal)
		}
		if c.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTP {
			t.Errorf("readiness scheme: expected HTTP, got %s", c.ReadinessProbe.HTTPGet.Scheme)
		}
		if c.LivenessProbe.HTTPGet.Path != "/app-health/app/livez" {
			t.Errorf("liveness path: expected /app-health/app/livez, got %s", c.LivenessProbe.HTTPGet.Path)
		}
	})

	t.Run("TCPSocket probes converted to HTTP", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)},
							},
						},
					},
				},
			},
		}

		patchRewriteProbe(pod, 15020, ProxyContainerName)

		c := pod.Spec.Containers[0]
		if c.ReadinessProbe.HTTPGet == nil {
			t.Error("expected TCP probe converted to HTTPGet")
		}
		if c.ReadinessProbe.TCPSocket != nil {
			t.Error("expected TCPSocket to be nil after conversion")
		}
	})

	t.Run("GRPC probes converted to HTTP", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								GRPC: &corev1.GRPCAction{Port: 50051},
							},
						},
					},
				},
			},
		}

		patchRewriteProbe(pod, 15020, ProxyContainerName)

		c := pod.Spec.Containers[0]
		if c.ReadinessProbe.HTTPGet == nil {
			t.Error("expected GRPC probe converted to HTTPGet")
		}
		if c.ReadinessProbe.GRPC != nil {
			t.Error("expected GRPC to be nil after conversion")
		}
	})

	t.Run("skip proxy container", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: ProxyContainerName,
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(15020)},
							},
						},
					},
				},
			},
		}

		originalPath := pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path
		patchRewriteProbe(pod, 15020, ProxyContainerName)

		if pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path != originalPath {
			t.Error("proxy container probe should not be rewritten")
		}
	})

	t.Run("init containers rewritten", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{
						Name: "init",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(9090)},
							},
						},
					},
				},
			},
		}

		patchRewriteProbe(pod, 15020, ProxyContainerName)

		c := pod.Spec.InitContainers[0]
		if c.ReadinessProbe.HTTPGet.Path != "/app-health/init/readyz" {
			t.Errorf("init readiness path: expected /app-health/init/readyz, got %s", c.ReadinessProbe.HTTPGet.Path)
		}
	})

	t.Run("lifecycle handlers converted", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						Lifecycle: &corev1.Lifecycle{
							PreStop: &corev1.LifecycleHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)},
							},
							PostStart: &corev1.LifecycleHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/start", Port: intstr.FromInt32(8080)},
							},
						},
					},
				},
			},
		}

		patchRewriteProbe(pod, 15020, ProxyContainerName)

		c := pod.Spec.Containers[0]
		if c.Lifecycle.PreStop.HTTPGet == nil {
			t.Error("expected PreStop TCPSocket converted to HTTPGet")
		}
		if c.Lifecycle.PreStop.TCPSocket != nil {
			t.Error("expected PreStop TCPSocket nil after conversion")
		}
		if c.Lifecycle.PostStart.HTTPGet.Path != "/app-lifecycle/app/poststartz" {
			t.Errorf("PostStart path: expected /app-lifecycle/app/poststartz, got %s", c.Lifecycle.PostStart.HTTPGet.Path)
		}
	})
}

func TestApplyHealthProbeRewrite(t *testing.T) {
	t.Run("no sidecar - no changes", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					HealthProbeRewriteAnnotation: "true",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080)},
							},
						},
					},
				},
			},
		}

		originalPath := pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path
		err := ApplyHealthProbeRewrite(pod)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Without sidecar, probe should not be rewritten
		if pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path != originalPath {
			t.Error("probe should not be rewritten without sidecar")
		}
	})

	t.Run("with sidecar - probe rewritten and env set", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					HealthProbeRewriteAnnotation: "true",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080)},
							},
						},
					},
					{
						Name: ProxyContainerName,
					},
				},
			},
		}

		err := ApplyHealthProbeRewrite(pod)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Check probe was rewritten
		c := pod.Spec.Containers[0]
		if c.ReadinessProbe.HTTPGet.Path != "/app-health/app/readyz" {
			t.Errorf("probe path: expected /app-health/app/readyz, got %s", c.ReadinessProbe.HTTPGet.Path)
		}

		// Check env was set on sidecar
		sidecar := pod.Spec.Containers[1]
		found := false
		for _, env := range sidecar.Env {
			if env.Name == KubeAppProberEnv {
				found = true
				if env.Value == "" {
					t.Error("ISTIO_KUBE_APP_PROBERS env should not be empty")
				}
				// Verify it's valid JSON
				var probers KubeAppProbers
				if err := json.Unmarshal([]byte(env.Value), &probers); err != nil {
					t.Errorf("ISTIO_KUBE_APP_PROBERS is not valid JSON: %v", err)
				}
			}
		}
		if !found {
			t.Error("expected ISTIO_KUBE_APP_PROBERS env on sidecar")
		}
	})

	t.Run("with sidecar but no probes - only rewrite applied", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					HealthProbeRewriteAnnotation: "true",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app"},
					{Name: ProxyContainerName},
				},
			},
		}

		err := ApplyHealthProbeRewrite(pod)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// No probes to dump, so env should not be set
		sidecar := pod.Spec.Containers[1]
		for _, env := range sidecar.Env {
			if env.Name == KubeAppProberEnv {
				t.Error("ISTIO_KUBE_APP_PROBERS should not be set when there are no probes")
			}
		}
	})

	t.Run("sidecar as init container", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					HealthProbeRewriteAnnotation: "true",
				},
			},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080)},
							},
						},
					},
					{
						Name: ProxyContainerName,
					},
				},
			},
		}

		err := ApplyHealthProbeRewrite(pod)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Check probe was rewritten
		c := pod.Spec.InitContainers[0]
		if c.ReadinessProbe.HTTPGet.Path != "/app-health/app/readyz" {
			t.Errorf("probe path: expected /app-health/app/readyz, got %s", c.ReadinessProbe.HTTPGet.Path)
		}

		// Check env was set on sidecar init container
		sidecar := pod.Spec.InitContainers[1]
		found := false
		for _, env := range sidecar.Env {
			if env.Name == KubeAppProberEnv {
				found = true
			}
		}
		if !found {
			t.Error("expected ISTIO_KUBE_APP_PROBERS env on sidecar init container")
		}
	})

	t.Run("annotation disables rewrite", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					HealthProbeRewriteAnnotation: "false",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "app",
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080)},
							},
						},
					},
					{
						Name: ProxyContainerName,
					},
				},
			},
		}

		originalPath := pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path
		originalPort := pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Port.IntVal

		err := ApplyHealthProbeRewrite(pod)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Probe should NOT be rewritten when annotation is "false"
		if pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path != originalPath {
			t.Errorf("probe should not be rewritten with annotation=false, got path %s", pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path)
		}
		if pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Port.IntVal != originalPort {
			t.Errorf("probe port should not change with annotation=false, got %d", pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Port.IntVal)
		}

		// Env should not be set on sidecar
		sidecar := pod.Spec.Containers[1]
		for _, env := range sidecar.Env {
			if env.Name == KubeAppProberEnv {
				t.Error("ISTIO_KUBE_APP_PROBERS should not be set when annotation disables rewrite")
			}
		}
	})
}

func TestKubeProbeToInternalProber(t *testing.T) {
	t.Run("nil probe", func(t *testing.T) {
		if p := kubeProbeToInternalProber(nil); p != nil {
			t.Errorf("expected nil, got %v", p)
		}
	})

	t.Run("HTTPGet probe", func(t *testing.T) {
		probe := &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080)},
			},
			TimeoutSeconds: 5,
		}
		p := kubeProbeToInternalProber(probe)
		if p == nil || p.HTTPGet == nil {
			t.Fatal("expected HTTPGet prober")
		}
		if p.TimeoutSeconds != 5 {
			t.Errorf("expected timeout 5, got %d", p.TimeoutSeconds)
		}
	})

	t.Run("TCPSocket probe", func(t *testing.T) {
		probe := &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)},
			},
			TimeoutSeconds: 3,
		}
		p := kubeProbeToInternalProber(probe)
		if p == nil || p.TCPSocket == nil {
			t.Fatal("expected TCPSocket prober")
		}
		if p.TimeoutSeconds != 3 {
			t.Errorf("expected timeout 3, got %d", p.TimeoutSeconds)
		}
	})

	t.Run("GRPC probe", func(t *testing.T) {
		probe := &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				GRPC: &corev1.GRPCAction{Port: 50051},
			},
			TimeoutSeconds: 10,
		}
		p := kubeProbeToInternalProber(probe)
		if p == nil || p.GRPC == nil {
			t.Fatal("expected GRPC prober")
		}
		if p.TimeoutSeconds != 10 {
			t.Errorf("expected timeout 10, got %d", p.TimeoutSeconds)
		}
	})
}

func TestKubeLifecycleHandlerToInternalProber(t *testing.T) {
	t.Run("nil handler", func(t *testing.T) {
		if p := kubeLifecycleHandlerToInternalProber(nil); p != nil {
			t.Errorf("expected nil, got %v", p)
		}
	})

	t.Run("HTTPGet handler", func(t *testing.T) {
		h := &corev1.LifecycleHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/shutdown", Port: intstr.FromInt32(8080)},
		}
		p := kubeLifecycleHandlerToInternalProber(h)
		if p == nil || p.HTTPGet == nil {
			t.Fatal("expected HTTPGet prober")
		}
	})

	t.Run("TCPSocket handler", func(t *testing.T) {
		h := &corev1.LifecycleHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)},
		}
		p := kubeLifecycleHandlerToInternalProber(h)
		if p == nil || p.TCPSocket == nil {
			t.Fatal("expected TCPSocket prober")
		}
	})

	t.Run("empty handler", func(t *testing.T) {
		h := &corev1.LifecycleHandler{}
		p := kubeLifecycleHandlerToInternalProber(h)
		if p != nil {
			t.Errorf("expected nil for empty handler, got %v", p)
		}
	})
}

func TestAllContainers(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init-1"}, {Name: "init-2"}},
			Containers:     []corev1.Container{{Name: "app"}},
		},
	}

	result := allContainers(pod)
	if len(result) != 3 {
		t.Errorf("expected 3 containers, got %d", len(result))
	}

	// Verify init containers come first
	if result[0].Name != "init-1" || result[1].Name != "init-2" {
		t.Errorf("expected init containers first, got %v", result)
	}
	if result[2].Name != "app" {
		t.Errorf("expected app container last, got %v", result)
	}
}
