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

package sidecarutils

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	trafficproxy "github.com/openkruise/agents/pkg/utils/sidecarutils/traffic-proxy"
)

const (
	testLegacyRuntimeTemplate      = `{"mainContainer":{"env":[{"name":"ENVD_DIR","value":"/mnt/envd"}]},"csiSidecar":[{"name":"agent-runtime","image":"runtime:legacy"}],"volume":[{"name":"envd-volume","emptyDir":{}}]}`
	testUserRuntimeTemplate        = `{"mainContainer":{},"csiSidecar":[{"name":"agent-runtime","image":"runtime:user"}],"volume":[]}`
	testLegacyCSITemplate          = `{"mainContainer":{},"csiSidecar":[{"name":"csi-sidecar","image":"csi:legacy"},{"name":"csi-agent-sidecar","image":"csi-agent:legacy"}],"volume":[{"name":"mount-root","emptyDir":{}}]}`
	testLegacyTrafficProxyTemplate = `{"initContainers":[{"name":"istio-init","image":"proxy:legacy"}]}`
	testRuntimeAIOTemplate         = `{
		"annotations": {
			"networking.agents.kruise.io/health-probe-rewrite": "true",
			"networking.agents.kruise.io/sidecar-proxy": "agent-runtime"
		},
		"labels": {"networking.istio.io/tunnel": "http"},
		"mainContainer": {
			"env": [{"name": "ENVD_DIR", "value": "/mnt/envd"}],
			"volumeMounts": [{"name": "envd-volume", "mountPath": "/mnt/envd"}],
			"lifecycle": {"postStart": {"exec": {"command": ["bash", "/mnt/envd/envd-run.sh"]}}}
		},
		"initContainers": [{
			"name": "agent-runtime",
			"image": "runtimed:aio",
			"restartPolicy": "Always",
			"env": [{"name": "RUNTIMED_ENABLED_SERVICES", "valueFrom": {"fieldRef": {"fieldPath": "metadata.annotations['agents.kruise.io/runtimed-plugins']"}}}]
		}],
		"volume": [{"name": "envd-volume", "emptyDir": {}}, {"name": "mount-root", "emptyDir": {}}]
	}`
	// testClusterRuntimeAIOTemplate mirrors the runtime-aio value deployed in
	// the cluster, with its repetitive per-service env entries trimmed.
	testClusterRuntimeAIOTemplate = `{
		"annotations": {
			"networking.agents.kruise.io/health-probe-rewrite": "true",
			"networking.agents.kruise.io/sidecar-proxy": "agent-runtime"
		},
		"labels": {
			"networking.agents.kruise.io/proxy-type": "ztunnel",
			"networking.istio.io/tunnel": "http",
			"security.istio.io/tlsMode": "disabled"
		},
		"mainContainer": {
			"env": [
				{"name": "ENVD_DIR", "value": "/mnt/envd"},
				{"name": "GODEBUG", "value": "multipathtcp=0"},
				{"name": "POD_UID", "valueFrom": {"fieldRef": {"fieldPath": "metadata.uid"}}}
			],
			"lifecycle": {"postStart": {"exec": {"command": ["bash", "/mnt/envd/envd-run.sh"]}}},
			"volumeMounts": [
				{"name": "envd-volume", "mountPath": "/mnt/envd"},
				{"name": "agent-runtime-state", "mountPath": "/var/run/agent-runtime"},
				{"name": "token-volume", "mountPath": "/var/opt/sandbox/agent-token"},
				{"name": "agent-helper", "mountPath": "/var/run/agent-helper"},
				{"name": "mount-root", "mountPath": "/run/csi/mount-root", "mountPropagation": "HostToContainer"}
			]
		},
		"initContainers": [{
			"name": "agent-runtime",
			"image": "registry.example/agent-runtime:cluster",
			"imagePullPolicy": "IfNotPresent",
			"restartPolicy": "Always",
			"securityContext": {"privileged": true},
			"env": [
				{"name": "RUNTIMED_SERVICE_ENV__AGENT_RUNTIME__ENVD_DIR", "value": "/mnt/envd"},
				{"name": "RUNTIMED_ENABLED_SERVICES", "valueFrom": {"fieldRef": {"fieldPath": "metadata.annotations['agents.kruise.io/runtimed-plugins']"}}},
				{"name": "__IGNORE_RESOURCE__", "value": "true"},
				{"name": "RUNTIMED_SERVICE_ENV__AGENT_RUNTIME__POD_UID", "valueFrom": {"fieldRef": {"apiVersion": "v1", "fieldPath": "metadata.uid"}}}
			],
			"resources": {
				"limits": {"cpu": "1200m", "memory": "2176Mi"},
				"requests": {"cpu": "700m", "memory": "1216Mi"}
			},
			"startupProbe": {"exec": {"command": ["/usr/local/bin/runtimed", "probe", "startup"]}, "failureThreshold": 120, "periodSeconds": 1, "timeoutSeconds": 10},
			"readinessProbe": {"exec": {"command": ["/usr/local/bin/runtimed", "probe", "readiness"]}, "failureThreshold": 3, "periodSeconds": 10, "timeoutSeconds": 2},
			"livenessProbe": {"exec": {"command": ["/usr/local/bin/runtimed", "probe", "liveness"]}, "failureThreshold": 3, "periodSeconds": 10, "timeoutSeconds": 2},
			"ports": [{"name": "ztunnel-stats", "containerPort": 15020, "protocol": "TCP"}],
			"volumeMounts": [
				{"name": "envd-volume", "mountPath": "/mnt/envd"},
				{"name": "agent-helper", "mountPath": "/var/run/agent-helper"},
				{"name": "mount-root", "mountPath": "/run/csi/mount-root", "mountPropagation": "Bidirectional"}
			]
		}],
		"volume": [
			{"name": "envd-volume", "emptyDir": {}},
			{"name": "token-volume", "emptyDir": {}},
			{"name": "agent-runtime-state", "emptyDir": {}},
			{"name": "agent-helper", "emptyDir": {}},
			{"name": "mount-root", "hostPath": {"path": "/var/run/csi", "type": "DirectoryOrCreate"}},
			{"name": "sandbox-ca-certs", "configMap": {"name": "sandbox-ca-certs", "optional": true}}
		]
	}`
)

// legacyTemplatesWith returns the legacy templates of the built-in runtimes
// together with extra.
func legacyTemplatesWith(extra map[string]string) map[string]string {
	data := map[string]string{
		KEY_RUNTIME_INJECTION_CONFIG:       testLegacyRuntimeTemplate,
		KEY_CSI_INJECTION_CONFIG:           testLegacyCSITemplate,
		KEY_TRAFFIC_PROXY_INJECTION_CONFIG: testLegacyTrafficProxyTemplate,
	}
	maps.Copy(data, extra)
	return data
}

func TestInjectSandboxRuntimesWithOptions_RuntimeInjectionProfile(t *testing.T) {
	agentRuntime := agentsv1alpha1.RuntimeConfigForInjectAgentRuntime
	csi := agentsv1alpha1.RuntimeConfigForInjectCsiMount
	trafficProxy := agentsv1alpha1.RuntimeConfigForInjectTrafficProxy

	legacyTemplates := legacyTemplatesWith(nil)
	aioTemplates := legacyTemplatesWith(map[string]string{KeyRuntimeAIOInjectionConfig: testRuntimeAIOTemplate})
	disabledAIOTemplates := legacyTemplatesWith(map[string]string{
		KeyRuntimeAIOInjectionConfig: testRuntimeAIOTemplate,
		KeyRuntimeAIODisabled:        "true",
	})
	aioInit := []string{"agent-runtime=runtimed:aio"}
	legacyInit := []string{"agent-runtime=runtime:legacy", "csi-sidecar=csi:legacy", "csi-agent-sidecar=csi-agent:legacy"}

	tests := []struct {
		name              string
		runtimes          []string
		podAnnotations    map[string]string
		podInitContainers []corev1.Container
		defaultData       map[string]string // nil: the default ConfigMap does not exist
		userData          map[string]string // nil: the user ConfigMap does not exist
		gateOff           bool
		// resumeProfile is the profile detected from a checkpoint delta on
		// resume; empty selects it from the configuration.
		resumeProfile string
		setup         func(t *testing.T)
		wantErr       error
		// wantErrContains is checked when the error has no sentinel to match.
		wantErrContains string
		wantReason      string
		wantServices    string
		wantInit        []string // name=image of the init containers, in order
		validate        func(t *testing.T, pod *corev1.Pod)
	}{
		{
			name:         "new pod selects all-in-one",
			runtimes:     []string{agentRuntime, csi},
			defaultData:  aioTemplates,
			wantServices: "agent-runtime,csi,csi-agent",
			wantInit:     aioInit,
			validate: func(t *testing.T, pod *corev1.Pod) {
				main := pod.Spec.Containers[0]
				if len(main.Env) != 1 || main.Env[0].Name != "ENVD_DIR" {
					t.Errorf("main container env = %v, want only ENVD_DIR", main.Env)
				}
				if len(main.VolumeMounts) != 1 || main.VolumeMounts[0].MountPath != "/mnt/envd" {
					t.Errorf("main container mounts = %v, want only /mnt/envd", main.VolumeMounts)
				}
				var volumes []string
				for _, v := range pod.Spec.Volumes {
					volumes = append(volumes, v.Name)
				}
				if !slices.Equal(volumes, []string{"mount-root", "envd-volume"}) {
					t.Errorf("volumes = %v, want the pod's mount-root kept and envd-volume added", volumes)
				}
				if main.Lifecycle == nil || main.Lifecycle.PostStart == nil || main.Lifecycle.PostStart.Exec == nil {
					t.Error("main container lacks the template postStart hook")
				}
				if pod.Labels["networking.istio.io/tunnel"] != "http" {
					t.Errorf("labels = %v, want the template labels applied as is", pod.Labels)
				}
				if pod.Annotations[trafficproxy.SidecarProxyAnnotation] != ContainerNameRuntimeAgent {
					t.Errorf("sidecar proxy = %q, want the template annotation applied as is",
						pod.Annotations[trafficproxy.SidecarProxyAnnotation])
				}
				if port := main.ReadinessProbe.HTTPGet.Port.IntValue(); port != 8080 {
					t.Errorf("main readiness probe port = %d, want it left alone without traffic-proxy", port)
				}
			},
		},
		{
			name:         "cluster shaped template keeps the injected container spec",
			runtimes:     []string{agentRuntime, csi},
			defaultData:  legacyTemplatesWith(map[string]string{KeyRuntimeAIOInjectionConfig: testClusterRuntimeAIOTemplate}),
			wantServices: "agent-runtime,csi,csi-agent",
			wantInit:     []string{"agent-runtime=registry.example/agent-runtime:cluster"},
			validate: func(t *testing.T, pod *corev1.Pod) {
				aio := pod.Spec.InitContainers[0]
				var servicesEnv *corev1.EnvVar
				for i := range aio.Env {
					if aio.Env[i].Name == "RUNTIMED_ENABLED_SERVICES" {
						servicesEnv = &aio.Env[i]
					}
				}
				if servicesEnv == nil || servicesEnv.ValueFrom == nil || servicesEnv.ValueFrom.FieldRef == nil {
					t.Errorf("AIO container services env = %v, want the downward API reference kept", servicesEnv)
				}
				if aio.RestartPolicy == nil || *aio.RestartPolicy != corev1.ContainerRestartPolicyAlways {
					t.Errorf("AIO container restart policy = %v, want Always", aio.RestartPolicy)
				}
				if aio.SecurityContext == nil || aio.SecurityContext.Privileged == nil || !*aio.SecurityContext.Privileged {
					t.Error("AIO container is not privileged")
				}
				if aio.StartupProbe == nil || aio.ReadinessProbe == nil || aio.LivenessProbe == nil {
					t.Error("AIO container lost its probes")
				}
				if got := aio.Resources.Limits.Cpu().String(); got != "1200m" {
					t.Errorf("AIO container cpu limit = %s, want 1200m", got)
				}

				main := pod.Spec.Containers[0]
				if len(main.Env) != 3 {
					t.Errorf("main container env = %v, want the three template envs", main.Env)
				}
				if main.Lifecycle == nil || main.Lifecycle.PostStart == nil || main.Lifecycle.PostStart.Exec == nil ||
					!slices.Equal(main.Lifecycle.PostStart.Exec.Command, []string{"bash", "/mnt/envd/envd-run.sh"}) {
					t.Errorf("main container postStart = %v, want the template hook", main.Lifecycle)
				}
				var mainMounts []string
				for _, mount := range main.VolumeMounts {
					mainMounts = append(mainMounts, mount.Name)
				}
				wantMounts := []string{"envd-volume", "agent-runtime-state", "token-volume", "agent-helper", "mount-root"}
				if !slices.Equal(mainMounts, wantMounts) {
					t.Errorf("main container mounts = %v, want %v", mainMounts, wantMounts)
				}

				mountRootCount := 0
				for _, vol := range pod.Spec.Volumes {
					if vol.Name == "mount-root" {
						mountRootCount++
					}
				}
				if mountRootCount != 1 {
					t.Errorf("mount-root appears %d times, want the pod's own volume reused", mountRootCount)
				}
				if !findVolumeByName(pod.Spec.Volumes, "envd-volume") || !findVolumeByName(pod.Spec.Volumes, "sandbox-ca-certs") {
					t.Errorf("volumes = %v, want the template volumes injected", pod.Spec.Volumes)
				}
				if pod.Annotations[trafficproxy.SidecarProxyAnnotation] != ContainerNameRuntimeAgent {
					t.Errorf("sidecar proxy = %q, want the template annotation", pod.Annotations[trafficproxy.SidecarProxyAnnotation])
				}
				if pod.Labels["security.istio.io/tlsMode"] != "disabled" {
					t.Errorf("labels = %v, want the template labels", pod.Labels)
				}
				if port := main.ReadinessProbe.HTTPGet.Port.IntValue(); port != 8080 {
					t.Errorf("main readiness probe port = %d, want it left alone without traffic-proxy", port)
				}
			},
		},
		{
			name:         "new pod with agent-runtime alone runs the runtime service only",
			runtimes:     []string{agentRuntime},
			defaultData:  aioTemplates,
			wantServices: "agent-runtime",
			wantInit:     aioInit,
		},
		{
			name:        "gate off keeps new pods legacy",
			runtimes:    []string{agentRuntime, csi},
			defaultData: aioTemplates,
			gateOff:     true,
			wantInit:    legacyInit,
		},
		{
			name:        "disabled all-in-one keeps new pods legacy",
			runtimes:    []string{agentRuntime, csi},
			defaultData: disabledAIOTemplates,
			wantInit:    legacyInit,
		},
		{
			name:        "missing runtime-aio template fails instead of falling back",
			runtimes:    []string{agentRuntime, csi},
			defaultData: legacyTemplates,
			wantErr:     ErrRuntimeAIOInjection,
		},
		{
			name:        "sandbox without agent-runtime stays legacy",
			runtimes:    []string{csi},
			defaultData: aioTemplates,
			wantInit:    []string{"csi-sidecar=csi:legacy", "csi-agent-sidecar=csi-agent:legacy"},
		},
		{
			name:        "user override of a used runtime template keeps legacy",
			runtimes:    []string{agentRuntime, csi},
			defaultData: aioTemplates,
			userData:    map[string]string{KEY_RUNTIME_INJECTION_CONFIG: testUserRuntimeTemplate},
			wantReason:  "user injection configuration overrides agent-runtime",
			wantInit:    []string{"agent-runtime=runtime:user", "csi-sidecar=csi:legacy", "csi-agent-sidecar=csi-agent:legacy"},
		},
		{
			name:         "user override of an unused runtime template allows all-in-one",
			runtimes:     []string{agentRuntime},
			defaultData:  aioTemplates,
			userData:     map[string]string{KEY_CSI_INJECTION_CONFIG: ""},
			wantServices: "agent-runtime",
			wantInit:     aioInit,
		},
		{
			name:        "user runtime-aio template is used",
			runtimes:    []string{agentRuntime},
			defaultData: aioTemplates,
			userData: map[string]string{
				KeyRuntimeAIOInjectionConfig: strings.Replace(testRuntimeAIOTemplate, "runtimed:aio", "runtimed:user", 1),
			},
			wantServices: "agent-runtime",
			wantInit:     []string{"agent-runtime=runtimed:user"},
		},
		{
			name:              "pod template with a runtime container keeps legacy",
			runtimes:          []string{agentRuntime, csi},
			podInitContainers: []corev1.Container{{Name: ContainerNameCSIAgentSidecar, Image: "csi-agent:custom"}},
			defaultData:       aioTemplates,
			wantReason:        "pod template already defines container csi-agent-sidecar",
			wantInit:          []string{"csi-agent-sidecar=csi-agent:custom", "agent-runtime=runtime:legacy"},
		},
		{
			name:     "literal service list in the template is replaced by the sandbox services",
			runtimes: []string{agentRuntime, csi},
			defaultData: legacyTemplatesWith(map[string]string{
				KeyRuntimeAIOInjectionConfig: `{"initContainers":[{"name":"agent-runtime","image":"runtimed:aio","restartPolicy":"Always","env":[{"name":"RUNTIMED_ENABLED_SERVICES","value":"all"}]}]}`,
			}),
			wantServices: "agent-runtime,csi,csi-agent",
			wantInit:     aioInit,
			validate: func(t *testing.T, pod *corev1.Pod) {
				if got := pod.Spec.InitContainers[0].Env[0].Value; got != "agent-runtime,csi,csi-agent" {
					t.Errorf("env value = %q, want it replaced by the sandbox services", got)
				}
			},
		},
		{
			name:     "extra containers in the template are injected as is",
			runtimes: []string{agentRuntime},
			defaultData: legacyTemplatesWith(map[string]string{
				KeyRuntimeAIOInjectionConfig: strings.Replace(testRuntimeAIOTemplate, `"initContainers": [{`,
					`"containers": [{"name": "aio-extra", "image": "extra:v1"}], "initContainers": [{"name": "aio-init", "image": "init:v1"}, {`, 1),
			}),
			wantServices: "agent-runtime",
			wantInit:     []string{"aio-init=init:v1", "agent-runtime=runtimed:aio"},
			validate: func(t *testing.T, pod *corev1.Pod) {
				if len(pod.Spec.Containers) != 2 || pod.Spec.Containers[1].Name != "aio-extra" {
					t.Errorf("containers = %v, want the template container appended", pod.Spec.Containers)
				}
			},
		},
		{
			name:         "traffic-proxy runs as a service of the all-in-one container",
			runtimes:     []string{agentRuntime, trafficProxy},
			defaultData:  aioTemplates,
			wantServices: "agent-runtime,traffic-proxy",
			wantInit:     aioInit,
			validate: func(t *testing.T, pod *corev1.Pod) {
				if pod.Labels["networking.istio.io/tunnel"] != "http" {
					t.Errorf("labels = %v, want the traffic-proxy labels of the template", pod.Labels)
				}
				if pod.Annotations[trafficproxy.SidecarProxyAnnotation] != ContainerNameRuntimeAgent {
					t.Errorf("sidecar proxy = %q, want %s", pod.Annotations[trafficproxy.SidecarProxyAnnotation], ContainerNameRuntimeAgent)
				}
				if port := pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Port.IntValue(); port != 15020 {
					t.Errorf("main readiness probe port = %d, want it rewritten to 15020", port)
				}
				if !findEnvByName(pod.Spec.InitContainers[0].Env, trafficproxy.KubeAppProberEnv) {
					t.Errorf("all-in-one container lacks %s", trafficproxy.KubeAppProberEnv)
				}
			},
		},
		{
			name:     "custom runtimes keep their own templates next to all-in-one",
			runtimes: []string{agentRuntime, "my-runtime"},
			defaultData: legacyTemplatesWith(map[string]string{
				KeyRuntimeAIOInjectionConfig: testRuntimeAIOTemplate,
				"my-runtime":                 `{"initContainers":[{"name":"my-init","image":"my:v1"}]}`,
			}),
			wantServices: "agent-runtime",
			wantInit:     []string{"agent-runtime=runtimed:aio", "my-init=my:v1"},
		},
		{
			name:     "missing custom runtime template fails",
			runtimes: []string{agentRuntime, "my-runtime"},
			defaultData: legacyTemplatesWith(map[string]string{
				KeyRuntimeAIOInjectionConfig: testRuntimeAIOTemplate,
			}),
			wantErrContains: "injection template my-runtime not found",
		},
		{
			name:        "CA bundles follow the runtimes the all-in-one container takes over",
			runtimes:    []string{agentRuntime, trafficProxy},
			defaultData: aioTemplates,
			setup: func(t *testing.T) {
				if err := utilfeature.DefaultMutableFeatureGate.Set("SecurityIdentityProvider=true"); err != nil {
					t.Fatalf("set feature gate: %v", err)
				}
				t.Cleanup(func() {
					_ = utilfeature.DefaultMutableFeatureGate.Set("SecurityIdentityProvider=false")
				})
				identity.BindCAEnabledFor(identity.GatewayCABundleName, func(sbx *agentsv1alpha1.Sandbox) bool {
					return IsRuntimeEnabled(sbx, agentsv1alpha1.RuntimeConfigForInjectTrafficProxy)
				})
				t.Cleanup(func() {
					identity.BindCAEnabledFor(identity.GatewayCABundleName, nil)
				})
			},
			wantServices: "agent-runtime,traffic-proxy",
			wantInit:     aioInit,
			validate: func(t *testing.T, pod *corev1.Pod) {
				if !findVolumeByName(pod.Spec.Volumes, "sandbox-gateway-ca") {
					t.Error("gateway CA enabled for traffic-proxy was not injected")
				}
			},
		},
		{
			name:          "resume keeps the all-in-one profile while the configuration disables it",
			runtimes:      []string{agentRuntime, csi},
			defaultData:   disabledAIOTemplates,
			resumeProfile: RuntimeInjectionProfileAllInOne,
			wantServices:  "agent-runtime,csi,csi-agent",
			wantInit:      aioInit,
		},
		{
			name:          "resume of an all-in-one sandbox fails with the gate off",
			runtimes:      []string{agentRuntime, csi},
			defaultData:   aioTemplates,
			gateOff:       true,
			resumeProfile: RuntimeInjectionProfileAllInOne,
			wantErr:       ErrRuntimeAIOInjection,
		},
		{
			name:          "resume keeps the legacy profile while the configuration selects all-in-one",
			runtimes:      []string{agentRuntime, csi},
			defaultData:   aioTemplates,
			resumeProfile: RuntimeInjectionProfileLegacy,
			wantInit:      legacyInit,
		},
		{
			name:         "resume without a profile selects it from the configuration",
			runtimes:     []string{agentRuntime, csi},
			defaultData:  aioTemplates,
			wantServices: "agent-runtime,csi,csi-agent",
			wantInit:     aioInit,
		},
		{
			name:          "resume of an all-in-one sandbox fails without the template",
			runtimes:      []string{agentRuntime},
			defaultData:   legacyTemplates,
			resumeProfile: RuntimeInjectionProfileAllInOne,
			wantErr:       ErrRuntimeAIOInjection,
		},
		{
			name:          "resume of an all-in-one sandbox fails without any configuration",
			runtimes:      []string{agentRuntime},
			resumeProfile: RuntimeInjectionProfileAllInOne,
			wantErr:       ErrRuntimeAIOInjection,
		},
		{
			name:          "resume of an all-in-one sandbox fails without agent-runtime",
			runtimes:      []string{csi},
			defaultData:   aioTemplates,
			resumeProfile: RuntimeInjectionProfileAllInOne,
			wantErr:       ErrRuntimeAIOInjection,
		},
		{
			name:              "resume of an all-in-one sandbox fails when the pod template defines agent-runtime",
			runtimes:          []string{agentRuntime},
			podInitContainers: []corev1.Container{{Name: ContainerNameRuntimeAgent, Image: "runtime:custom"}},
			defaultData:       aioTemplates,
			resumeProfile:     RuntimeInjectionProfileAllInOne,
			wantErr:           ErrRuntimeAIOInjection,
		},
		{
			name:     "runtime-aio template without the all-in-one container fails instead of falling back",
			runtimes: []string{agentRuntime},
			defaultData: legacyTemplatesWith(map[string]string{
				KeyRuntimeAIOInjectionConfig: `{"initContainers":[{"name":"agent-runtime","image":"runtimed:aio"}]}`,
			}),
			wantErr: ErrRuntimeAIOInjection,
		},
		{
			name:     "runtime-aio template declaring the container in csiSidecar fails",
			runtimes: []string{agentRuntime},
			defaultData: legacyTemplatesWith(map[string]string{
				KeyRuntimeAIOInjectionConfig: `{"csiSidecar":[{"name":"agent-runtime","image":"runtimed:aio","env":[{"name":"RUNTIMED_ENABLED_SERVICES","value":"all"}]}]}`,
			}),
			wantErr: ErrRuntimeAIOInjection,
		},
		{
			name:     "runtime-aio template without the native sidecar restart policy fails",
			runtimes: []string{agentRuntime},
			defaultData: legacyTemplatesWith(map[string]string{
				KeyRuntimeAIOInjectionConfig: strings.Replace(testRuntimeAIOTemplate, `"restartPolicy": "Always",`, "", 1),
			}),
			wantErr: ErrRuntimeAIOInjection,
		},
		{
			name:     "runtime-aio template reading services from another pod field fails",
			runtimes: []string{agentRuntime},
			defaultData: legacyTemplatesWith(map[string]string{
				KeyRuntimeAIOInjectionConfig: strings.Replace(testRuntimeAIOTemplate,
					"metadata.annotations['agents.kruise.io/runtimed-plugins']", "metadata.name", 1),
			}),
			wantErr: ErrRuntimeAIOInjection,
		},
		{
			name:     "runtime-aio template reading services from a config map fails",
			runtimes: []string{agentRuntime},
			defaultData: legacyTemplatesWith(map[string]string{
				KeyRuntimeAIOInjectionConfig: strings.Replace(testRuntimeAIOTemplate,
					`"fieldRef": {"fieldPath": "metadata.annotations['agents.kruise.io/runtimed-plugins']"}`,
					`"configMapKeyRef": {"name": "agent-runtime", "key": "services"}`, 1),
			}),
			wantErr: ErrRuntimeAIOInjection,
		},
		{
			name:        "unparsable runtime-aio template fails instead of falling back",
			runtimes:    []string{agentRuntime},
			defaultData: legacyTemplatesWith(map[string]string{KeyRuntimeAIOInjectionConfig: `{`}),
			wantErr:     ErrRuntimeAIOInjection,
		},
		{
			name:            "gate off without configuration reports the missing declared template",
			runtimes:        []string{agentRuntime},
			gateOff:         true,
			wantErrContains: "injection template agent-runtime not found",
		},
		{
			name:     "missing configuration fails instead of falling back",
			runtimes: []string{agentRuntime},
			wantErr:  ErrRuntimeAIOInjection,
		},
		{
			name: "injection annotations copied into the pod template are dropped",
			podAnnotations: map[string]string{
				AnnotationRuntimeInjectionProfileReason: "copied from a live pod",
				AnnotationRuntimedPlugins:               "agent-runtime",
			},
			defaultData: aioTemplates,
		},
		{
			name:          "resume of an all-in-one sandbox without any runtime fails",
			defaultData:   aioTemplates,
			resumeProfile: RuntimeInjectionProfileAllInOne,
			wantErr:       ErrRuntimeAIOInjection,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			sandbox := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
			for _, name := range tt.runtimes {
				sandbox.Spec.Runtimes = append(sandbox.Spec.Runtimes, agentsv1alpha1.RuntimeConfig{Name: name})
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Annotations: maps.Clone(tt.podAnnotations)},
				Spec: corev1.PodSpec{
					InitContainers: tt.podInitContainers,
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "nginx",
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8080)},
						}},
					}},
					Volumes: []corev1.Volume{{Name: "mount-root", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				},
			}
			var objs []ctrlclient.Object
			for name, data := range map[string]map[string]string{
				SandboxInjectionConfigName:     tt.defaultData,
				SandboxInjectionUserConfigName: tt.userData,
			} {
				if data != nil {
					objs = append(objs, &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: utils.DefaultSandboxDeployNamespace},
						Data:       data,
					})
				}
			}
			cli := fake.NewClientBuilder().WithObjects(objs...).Build()
			ctx := context.Background()

			err := InjectSandboxRuntimesWithOptions(ctx, sandbox, pod, cli, RuntimeInjectionOptions{
				AIOEnabled:    !tt.gateOff,
				ResumeProfile: tt.resumeProfile,
			})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if tt.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got := pod.Annotations[AnnotationRuntimeInjectionProfileReason]; got != tt.wantReason {
				t.Errorf("reason = %q, want %q", got, tt.wantReason)
			}
			if got := pod.Annotations[AnnotationRuntimedPlugins]; got != tt.wantServices {
				t.Errorf("services = %q, want %q", got, tt.wantServices)
			}
			var gotInit []string
			for _, c := range pod.Spec.InitContainers {
				gotInit = append(gotInit, c.Name+"="+c.Image)
			}
			if !slices.Equal(gotInit, tt.wantInit) {
				t.Errorf("init containers = %v, want %v", gotInit, tt.wantInit)
			}
			if tt.validate != nil {
				tt.validate(t, pod)
			}
		})
	}
}

func TestIsRuntimeAIOContainer(t *testing.T) {
	servicesEnv := corev1.EnvVar{Name: envRuntimedEnabledServices, Value: "agent-runtime"}
	downwardAPIEnv := corev1.EnvVar{
		Name: envRuntimedEnabledServices,
		ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
			FieldPath: runtimedPluginsFieldPath,
		}},
	}

	tests := []struct {
		name      string
		container corev1.Container
		want      bool
	}{
		{
			name:      "another container name",
			container: corev1.Container{Name: "main", Env: []corev1.EnvVar{servicesEnv}},
			want:      false,
		},
		{
			name:      "runtime container without the services env",
			container: corev1.Container{Name: ContainerNameRuntimeAgent, Env: []corev1.EnvVar{{Name: "OTHER", Value: "x"}}},
			want:      false,
		},
		{
			name:      "runtime container without env",
			container: corev1.Container{Name: ContainerNameRuntimeAgent},
			want:      false,
		},
		{
			name:      "literal services",
			container: corev1.Container{Name: ContainerNameRuntimeAgent, Env: []corev1.EnvVar{servicesEnv}},
			want:      true,
		},
		{
			name:      "downward API of the plugins annotation",
			container: corev1.Container{Name: ContainerNameRuntimeAgent, Env: []corev1.EnvVar{downwardAPIEnv}},
			want:      true,
		},
		{
			name: "downward API of the plugins annotation with explicit apiVersion",
			container: corev1.Container{Name: ContainerNameRuntimeAgent, Env: []corev1.EnvVar{{
				Name: envRuntimedEnabledServices,
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  runtimedPluginsFieldPath,
				}},
			}}},
			want: true,
		},
		{
			name: "downward API of another field",
			container: corev1.Container{Name: ContainerNameRuntimeAgent, Env: []corev1.EnvVar{{
				Name:      envRuntimedEnabledServices,
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
			}}},
			want: false,
		},
		{
			name: "downward API with an unsupported apiVersion",
			container: corev1.Container{Name: ContainerNameRuntimeAgent, Env: []corev1.EnvVar{{
				Name: envRuntimedEnabledServices,
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
					APIVersion: "v2",
					FieldPath:  runtimedPluginsFieldPath,
				}},
			}}},
			want: false,
		},
		{
			name: "configMap key reference",
			container: corev1.Container{Name: ContainerNameRuntimeAgent, Env: []corev1.EnvVar{{
				Name: envRuntimedEnabledServices,
				ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "agent-runtime"},
					Key:                  "services",
				}},
			}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRuntimeAIOContainer(&tt.container); got != tt.want {
				t.Errorf("IsRuntimeAIOContainer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRuntimeAIOContainerIndex(t *testing.T) {
	aio := corev1.Container{Name: ContainerNameRuntimeAgent, Env: []corev1.EnvVar{
		{Name: envRuntimedEnabledServices, Value: "agent-runtime"},
	}}
	init := corev1.Container{Name: "init", Image: "init:v1"}

	tests := []struct {
		name           string
		initContainers []corev1.Container
		want           int
	}{
		{name: "no containers", want: -1},
		{name: "no all-in-one container", initContainers: []corev1.Container{init}, want: -1},
		{name: "first container", initContainers: []corev1.Container{aio, init}, want: 0},
		{name: "among other containers", initContainers: []corev1.Container{init, aio}, want: 1},
		{name: "duplicate all-in-one containers", initContainers: []corev1.Container{aio, aio}, want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runtimeAIOContainerIndex(tt.initContainers); got != tt.want {
				t.Errorf("runtimeAIOContainerIndex() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSetEnabledServicesEnv(t *testing.T) {
	downwardAPIEnv := corev1.EnvVar{
		Name: envRuntimedEnabledServices,
		ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
			FieldPath: runtimedPluginsFieldPath,
		}},
	}

	t.Run("replaces a literal value", func(t *testing.T) {
		got := setEnabledServicesEnv([]corev1.EnvVar{
			{Name: "OTHER", Value: "x"},
			{Name: envRuntimedEnabledServices, Value: "all"},
		}, "agent-runtime,csi")
		if len(got) != 2 || got[0].Name != "OTHER" || got[0].Value != "x" {
			t.Fatalf("env = %v, want the unrelated env left alone", got)
		}
		if got[1].Value != "agent-runtime,csi" || got[1].ValueFrom != nil {
			t.Errorf("services env = %v, want the literal value replaced", got[1])
		}
	})

	t.Run("keeps the downward API reference", func(t *testing.T) {
		got := setEnabledServicesEnv([]corev1.EnvVar{downwardAPIEnv}, "agent-runtime")
		if len(got) != 1 || got[0].ValueFrom == nil || got[0].ValueFrom.FieldRef == nil {
			t.Fatalf("env = %v, want the downward API reference kept", got)
		}
		if got[0].Value != "" {
			t.Errorf("services env value = %q, want it left unset", got[0].Value)
		}
	})

	t.Run("appends the services env when missing", func(t *testing.T) {
		got := setEnabledServicesEnv([]corev1.EnvVar{{Name: "OTHER", Value: "x"}}, "agent-runtime,csi")
		if len(got) != 2 {
			t.Fatalf("env = %v, want the services env appended", got)
		}
		if got[1].Name != envRuntimedEnabledServices || got[1].Value != "agent-runtime,csi" {
			t.Errorf("appended env = %v, want %s=agent-runtime,csi", got[1], envRuntimedEnabledServices)
		}
	})
}

func TestParseRuntimeAIOConfig(t *testing.T) {
	valid := `{"initContainers":[{"name":"agent-runtime","image":"runtimed:aio","restartPolicy":"Always","env":[{"name":"RUNTIMED_ENABLED_SERVICES","valueFrom":{"fieldRef":{"fieldPath":"metadata.annotations['agents.kruise.io/runtimed-plugins']"}}}]}]}`

	tests := []struct {
		name      string
		configs   map[string]string
		wantErr   string
		wantIndex int
	}{
		{
			name:      "valid template",
			configs:   map[string]string{KeyRuntimeAIOInjectionConfig: valid},
			wantIndex: 0,
		},
		{
			name:    "missing template",
			configs: map[string]string{},
			wantErr: "injection template runtime-aio not found",
		},
		{
			name:    "unparsable template",
			configs: map[string]string{KeyRuntimeAIOInjectionConfig: `{`},
			wantErr: "failed to parse injection template runtime-aio",
		},
		{
			name: "template without the all-in-one container",
			configs: map[string]string{
				KeyRuntimeAIOInjectionConfig: `{"initContainers":[{"name":"init","image":"init:v1"}]}`,
			},
			wantErr: "must define exactly one init container",
		},
		{
			name: "template with two all-in-one containers",
			configs: map[string]string{
				KeyRuntimeAIOInjectionConfig: strings.Replace(valid, `"initContainers":[{"name":"agent-runtime"`,
					`"initContainers":[{"name":"agent-runtime","image":"runtimed:aio","restartPolicy":"Always","env":[{"name":"RUNTIMED_ENABLED_SERVICES","value":"agent-runtime"}]},{"name":"agent-runtime"`, 1),
			},
			wantErr: "must define exactly one init container",
		},
		{
			name: "container without restartPolicy Always",
			configs: map[string]string{
				KeyRuntimeAIOInjectionConfig: strings.Replace(valid, `"restartPolicy":"Always"`, `"restartPolicy":"Never"`, 1),
			},
			wantErr: "must declare init container agent-runtime with restartPolicy Always",
		},
		{
			name: "extra containers declared in csiSidecar",
			configs: map[string]string{
				KeyRuntimeAIOInjectionConfig: strings.Replace(valid, `"initContainers":`,
					`"csiSidecar":[{"name":"csi-sidecar","image":"csi:v1"}],"initContainers":`, 1),
			},
			wantErr: "must declare its containers in initContainers, not csiSidecar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := parseRuntimeAIOConfig(tt.configs)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := runtimeAIOContainerIndex(config.InitContainers); got != tt.wantIndex {
				t.Errorf("all-in-one container index = %d, want %d", got, tt.wantIndex)
			}
		})
	}
}

func TestRuntimeInjectionProfileOfDelta(t *testing.T) {
	tests := []struct {
		name    string
		delta   string
		want    string
		wantErr bool
	}{
		{
			name:  "all-in-one container",
			delta: `{"spec":{"initContainers":[{"name":"agent-runtime","image":"runtimed:aio","env":[{"name":"RUNTIMED_ENABLED_SERVICES","value":"agent-runtime"}]}],"$setElementOrder/initContainers":[{"name":"agent-runtime"}]}}`,
			want:  RuntimeInjectionProfileAllInOne,
		},
		{
			name:  "legacy containers",
			delta: `{"spec":{"initContainers":[{"name":"agent-runtime","image":"runtime:legacy"},{"name":"csi-sidecar","image":"csi:legacy"}]}}`,
			want:  RuntimeInjectionProfileLegacy,
		},
		{
			name:  "all-in-one container among regular containers",
			delta: `{"spec":{"containers":[{"name":"agent-runtime","env":[{"name":"RUNTIMED_ENABLED_SERVICES","value":"agent-runtime"}]}]}}`,
			want:  RuntimeInjectionProfileAllInOne,
		},
		{
			name:  "no agent-runtime container",
			delta: `{"spec":{"containers":[{"name":"main","resources":{"limits":{"cpu":"2"}}}]}}`,
			want:  "",
		},
		{
			name:    "unparsable delta",
			delta:   `{"spec":`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RuntimeInjectionProfileOfDelta([]byte(tt.delta))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("profile = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInjectSandboxRuntimesWithOptionsConfigurationError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	configErr := fmt.Errorf("configmap lookup failed")
	cli := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ ctrlclient.WithWatch, _ ctrlclient.ObjectKey, _ ctrlclient.Object, _ ...ctrlclient.GetOption) error {
			return configErr
		},
	}).Build()

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxSpec{
			Runtimes: []agentsv1alpha1.RuntimeConfig{{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime}},
		},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}

	err := InjectSandboxRuntimesWithOptions(context.Background(), sandbox, pod, cli, RuntimeInjectionOptions{AIOEnabled: true})
	if !errors.Is(err, configErr) {
		t.Fatalf("error = %v, want %v", err, configErr)
	}
}
