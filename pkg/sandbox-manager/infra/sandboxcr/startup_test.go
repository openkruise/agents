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

package sandboxcr

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	testutils "github.com/openkruise/agents/test/utils"
	"io"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/runtime"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
)

func TestCloneDeferredStartupDelivery(t *testing.T) {
	processServer := testutils.NewTestRuntimeServer(testutils.TestRuntimeServerOptions{RunCommandImmediately: true, RunCommandResult: runtime.RunCommandResult{Exited: true}})
	defer processServer.Close()
	var mu sync.Mutex
	files := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/files" {
			processServer.Config.Handler.ServeHTTP(w, r)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		files[r.URL.Query().Get("path")] = string(data)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	request, err := json.Marshal(runtimeconfig.InitRuntimeOptions{SkipInit: true, StartupDelivered: true, AccessToken: runtime.AccessToken, EnvVars: map[string]string{"VALUE": "restored"}, Command: []string{"printf", "%s", "restored-command"}})
	require.NoError(t, err)
	template := &v1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "checkpoint", Namespace: "default"}, Spec: v1alpha1.SandboxTemplateSpec{StartupMode: v1alpha1.StartupModeDeferred, Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "app:test"}}}}}}
	checkpoint := &v1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: template.Name, Namespace: "default", Labels: map[string]string{v1alpha1.LabelSandboxTemplate: template.Name}, Annotations: map[string]string{v1alpha1.AnnotationInitRuntimeRequest: string(request)}}, Status: v1alpha1.CheckpointStatus{CheckpointId: "checkpoint"}}
	cache, cli, err := cachetest.NewTestCache(t, template, checkpoint)
	require.NoError(t, err)
	originalCreate := DefaultCreateSandbox
	t.Cleanup(func() { DefaultCreateSandbox = originalCreate })
	DefaultCreateSandbox = func(ctx context.Context, box *v1alpha1.Sandbox, c client.Client) (*v1alpha1.Sandbox, error) {
		box.Annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
		created, err := originalCreate(ctx, box, c)
		if err != nil {
			return nil, err
		}
		created.Status = v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning, PodInfo: v1alpha1.PodInfo{PodIP: "127.0.0.1", PodUID: "clone-pod-uid"}, Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}}
		if err := c.Status().Update(ctx, created); err != nil {
			return nil, err
		}
		return created, nil
	}
	cloned, _, err := CloneSandbox(t.Context(), infra.CloneSandboxOptions{User: "owner", CheckPointID: "checkpoint", Namespace: "default", WaitReadyTimeout: time.Second, CloneTimeout: time.Second}, cache)
	require.NoError(t, err)
	stored := &v1alpha1.Sandbox{}
	require.NoError(t, cli.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: cloned.GetName()}, stored))
	assert.Equal(t, v1alpha1.StartupModeDeferred, stored.Spec.StartupMode)
	record, err := runtime.GetInitRuntimeRequest(stored)
	require.NoError(t, err)
	assert.True(t, record.StartupDelivered)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "clone-pod-uid", files["/mnt/envd/claim/.env_ready"])
	assert.Contains(t, files["/mnt/envd/claim/cmd.sh"], "restored-command")
	assert.Contains(t, files["/mnt/envd/claim/env.sh"], "export VALUE='restored'")
}

func TestResumeDeferredClaim(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("delivery-fails=%t", fail), func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				assert.Equal(t, "/files", r.URL.Path)
				if fail {
					http.Error(w, "failed", http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			record, err := json.Marshal(runtimeconfig.InitRuntimeOptions{SkipInit: true})
			require.NoError(t, err)
			box := &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "claimed", Namespace: "default", UID: "sandbox-uid", Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeURL: server.URL, v1alpha1.AnnotationInitRuntimeRequest: string(record),
			}, Labels: map[string]string{v1alpha1.LabelSandboxIsClaimed: v1alpha1.True}}, Spec: v1alpha1.SandboxSpec{StartupMode: v1alpha1.StartupModeDeferred},
				Status: v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning, PodInfo: v1alpha1.PodInfo{PodUID: "pod-uid", PodIP: "127.0.0.1"}, Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}}}
			cache, cli, err := cachetest.NewTestCache(t, box)
			require.NoError(t, err)
			err = ResumeDeferredClaim(t.Context(), box, infra.ClaimSandboxOptions{WaitReadyTimeout: time.Second}, cache)
			if fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			stored := &v1alpha1.Sandbox{}
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(box), stored))
			request, err := runtime.GetInitRuntimeRequest(stored)
			require.NoError(t, err)
			assert.Equal(t, !fail, request.StartupDelivered)
			if !fail {
				require.NoError(t, ResumeDeferredClaim(t.Context(), stored, infra.ClaimSandboxOptions{}, cache))
			}
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, 1, calls)
		})
	}
}

func TestClaimStartupPersistence(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mode    v1alpha1.StartupMode
		data    runtimeconfig.StartupData
		init    bool
		wantErr bool
	}{
		{name: "command and env survive skipped init", mode: v1alpha1.StartupModeDeferred, data: runtimeconfig.StartupData{Command: []string{"printf", "claim"}, EnvVars: map[string]string{"VALUE": "claim"}}},
		{name: "empty claim has a durable replay record", mode: v1alpha1.StartupModeDeferred},
		{name: "env with init", mode: v1alpha1.StartupModeDeferred, init: true, data: runtimeconfig.StartupData{EnvVars: map[string]string{"VALUE": "claim"}}},
		{name: "Immediate command fails", data: runtimeconfig.StartupData{Command: []string{"true"}}, wantErr: true},
		{name: "invalid input fails before claim mutation", mode: v1alpha1.StartupModeDeferred, data: runtimeconfig.StartupData{EnvVars: map[string]string{"UID": "1"}}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			box := &Sandbox{Sandbox: &v1alpha1.Sandbox{Spec: v1alpha1.SandboxSpec{StartupMode: tt.mode,
				EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Command: []string{"sleep", "infinity"}}}}}},
			}}}
			original := box.Sandbox.DeepCopy()
			opts := infra.ClaimSandboxOptions{StartupData: &tt.data}
			if tt.init {
				opts.InitRuntime = &runtimeconfig.InitRuntimeOptions{AccessToken: "test-token", EnvVars: tt.data.EnvVars}
			}
			err := modifyPickedSandbox(box, infra.LockTypeUpdate, opts)
			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, original, box.Sandbox)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, original.Spec.Template, box.Spec.Template)
			record, err := runtime.GetInitRuntimeRequest(box.Sandbox)
			require.NoError(t, err)
			require.NotNil(t, record)
			assert.Equal(t, tt.data.Command, record.Command)
			assert.Equal(t, tt.data.EnvVars, record.EnvVars)
			assert.Equal(t, !tt.init, record.SkipInit)
			assert.Equal(t, v1alpha1.True, box.Labels[v1alpha1.LabelSandboxIsClaimed])
		})
	}
}

func TestClaimStartupReleaseOrdering(t *testing.T) {
	for _, tt := range []struct {
		name           string
		mode           v1alpha1.StartupMode
		skipInit, fail bool
	}{
		{name: "init before signal", mode: v1alpha1.StartupModeDeferred},
		{name: "skipped init still signals", mode: v1alpha1.StartupModeDeferred, skipInit: true},
		{name: "Immediate does not signal"},
		{name: "delivery failure returned", mode: v1alpha1.StartupModeDeferred, fail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, r.URL.Path)
				if r.URL.Path == "/files" {
					if tt.fail {
						http.Error(w, "delivery failed", http.StatusForbidden)
						return
					}
					file, _, err := r.FormFile("file")
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					defer file.Close()
					body, err := io.ReadAll(file)
					assert.NoError(t, err)
					assert.Equal(t, "pod-uid", string(body))
					assert.Equal(t, "/mnt/envd/claim/.env_ready", r.URL.Query().Get("path"))
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			box := &Sandbox{Sandbox: &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1alpha1.AnnotationRuntimeURL: server.URL}}, Spec: v1alpha1.SandboxSpec{StartupMode: tt.mode}}}
			box.Status.PodInfo.PodUID = "pod-uid"
			opts := infra.ClaimSandboxOptions{}
			if !tt.skipInit {
				opts.InitRuntime = &runtimeconfig.InitRuntimeOptions{SkipRefresh: true}
			}
			err := runClaimPostProcesses(t.Context(), box, infra.LockTypeUpdate, opts, nil, &infra.ClaimMetrics{})
			mu.Lock()
			defer mu.Unlock()
			if tt.fail {
				require.ErrorContains(t, err, "deliver startup data")
			} else {
				require.NoError(t, err)
			}
			expected := []string{}
			if !tt.skipInit {
				expected = append(expected, "/init")
			}
			if tt.mode == v1alpha1.StartupModeDeferred {
				expected = append(expected, "/files")
			}
			assert.Equal(t, expected, calls)
		})
	}
}
