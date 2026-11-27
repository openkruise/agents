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

package sandboxtemplate

import (
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client"
)

func TestBuildControllerRegistration(t *testing.T) {
	// Registry has no reset API, so its mutation stays in a child test process.
	if os.Getenv("AGENTS_BUILD_REGISTRATION_CHILD") != "1" {
		require.Nil(t, client.GetGenericClient())
		mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:0"}, ctrl.Options{Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
		require.NoError(t, err)
		require.NoError(t, Add(mgr, Options{}))
		args := []string{"-test.run=^TestBuildControllerRegistration$", "-test.v"}
		if directory := flag.Lookup("test.gocoverdir"); directory != nil && directory.Value.String() != "" {
			args = append(args, "-test.gocoverdir="+directory.Value.String())
		}
		command := exec.CommandContext(t.Context(), os.Args[0], args...)
		command.Env = append(os.Environ(), "AGENTS_BUILD_REGISTRATION_CHILD=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		return
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/agents.kruise.io/v1alpha1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(metav1.APIResourceList{GroupVersion: api.GroupVersion.String(), APIResources: []metav1.APIResource{
			{Name: "sandboxtemplates", Kind: "SandboxTemplate", Namespaced: true},
		}})
		if err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	config := &rest.Config{Host: server.URL}
	require.NoError(t, client.NewRegistry(config))
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, api.AddToScheme(s))
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{api.GroupVersion, corev1.SchemeGroupVersion})
	for _, kind := range []string{"SandboxTemplate", "Sandbox", "Checkpoint"} {
		mapper.Add(api.GroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
	mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: s, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0", MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil }})
	require.NoError(t, err)
	require.NoError(t, Add(mgr, Options{}))
	require.Positive(t, requests.Load(), "registration must discover only the SandboxTemplate kind")
	require.ErrorContains(t, Add(mgr, Options{Timeout: DefaultBuildTimeout}), "sandbox-template-version", "duplicate controller name registration must fail")
}
