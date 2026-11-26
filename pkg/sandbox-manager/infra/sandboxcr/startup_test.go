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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/identity"
	claimengine "github.com/openkruise/agents/pkg/lifecycle/claim"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/runtime"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
	testutils "github.com/openkruise/agents/test/utils"
)

func TestCloneDeferredStartupDelivery(t *testing.T) {
	for _, tt := range []struct {
		name, fault string
		init        bool
		reserveFor  *time.Duration
	}{
		{name: "skipped init delivers restored data"},
		{name: "re-init and tokens and storage precede publication", init: true},
		{name: "commit response lost recovers exact Pod UID", fault: "commit-lost", init: true},
		{name: "skipped init response lost recovers", fault: "commit-lost"},
		{name: "cleanup failure recovers publication", fault: "cleanup-lost", init: true},
		{name: "commit uncertainty with denied read preserves clone", fault: "read-denied", init: true},
		{name: "commit uncertainty with stale UID preserves clone", fault: "read-mismatch", init: true},
		{name: "commit uncertainty with absent signal preserves clone", fault: "read-absent", init: true},
		{name: "uncertainty skips delayed shutdown", fault: "read-denied", init: true, reserveFor: ptr.To(time.Hour)},
		{name: "precommit env failure deletes clone", fault: "env.sh", init: true},
		{name: "precommit command failure deletes clone", fault: "cmd.sh", init: true},
		{name: "precommit signal failure deletes clone", fault: ".env_ready", init: true},
		{name: "completion bookkeeping failure preserves published clone", fault: "record-denied", init: true},
		{name: "recovered publication survives bookkeeping failure", fault: "commit-lost-record-denied", init: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := runtimeconfig.StartupData{EnvVars: map[string]string{"VALUE": "restored"}, Command: []string{"printf", "%s", "restored-command"}}
			rt := newStartupPublicationRuntime(t, tt.fault, "clone-pod-uid", data)
			request, err := json.Marshal(runtimeconfig.InitRuntimeOptions{
				SkipInit: !tt.init, StartupDelivered: true, StartupPodUID: "source-pod-uid",
				AccessToken: runtime.AccessToken, EnvVars: data.EnvVars, Command: data.Command,
			})
			require.NoError(t, err)
			template := &v1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "checkpoint", Namespace: "default"}, Spec: v1alpha1.SandboxTemplateSpec{StartupMode: v1alpha1.StartupModeDeferred, Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "app:test"}}}}}}
			checkpoint := &v1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: template.Name, Namespace: "default", Labels: map[string]string{v1alpha1.LabelSandboxTemplate: template.Name}, Annotations: map[string]string{v1alpha1.AnnotationInitRuntimeRequest: string(request)}}, Status: v1alpha1.CheckpointStatus{CheckpointId: "checkpoint"}}
			cache, cli, err := cachetest.NewTestCache(t, template, checkpoint)
			require.NoError(t, err)
			provider := rt.withCompletionChecks(cache, cli)
			originalCreate := DefaultCreateSandbox
			t.Cleanup(func() { DefaultCreateSandbox = originalCreate })
			creates := 0
			DefaultCreateSandbox = func(ctx context.Context, box *v1alpha1.Sandbox, c client.Client) (*v1alpha1.Sandbox, error) {
				creates++
				box.UID = "clone-sandbox-uid"
				box.Annotations[v1alpha1.AnnotationRuntimeURL] = rt.server.URL
				replay, err := runtime.GetInitRuntimeRequest(box)
				if err != nil {
					return nil, err
				}
				assert.False(t, replay.StartupDelivered, "source completion must not suppress clone publication")
				assert.Empty(t, replay.StartupPodUID)
				if !tt.init {
					assert.NotEqual(t, runtime.AccessToken, replay.AccessToken, "skipped-init clone gets its own generated token")
				}
				created, err := originalCreate(ctx, box, c)
				if err != nil {
					return nil, err
				}
				created.Status = v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning,
					ObservedGeneration: created.Generation, PodInfo: v1alpha1.PodInfo{PodIP: "127.0.0.1", PodUID: "clone-pod-uid"},
					Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "StartupDeliveryPending"}}}
				if err := c.Status().Update(ctx, created); err != nil {
					return nil, err
				}
				if err := c.Create(ctx, startupPublicationPod(created)); err != nil {
					return nil, err
				}
				return created, nil
			}
			opts, err := ValidateAndInitCloneOptions(infra.CloneSandboxOptions{
				User: "owner", Name: "cloned", CheckPointID: "checkpoint", Namespace: "default",
				WaitReadyTimeout: time.Second, CloneTimeout: time.Second, ReserveFailedSandboxFor: tt.reserveFor,
			})
			require.NoError(t, err)
			if tt.init {
				opts.CSIMount = rt.withIdentityAndStorage()
				opts.Modifier = func(box claimengine.Handle) error {
					annotations := box.GetAnnotations()
					annotations[identity.AnnotationAgentName] = "test-agent"
					annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
					box.SetAnnotations(annotations)
					return nil
				}
			}
			cloned, _, cloneErr := CloneSandbox(t.Context(), opts, provider)
			assert.Equal(t, 1, creates)
			rt.assertDeliveryResult(cli, client.ObjectKey{Namespace: "default", Name: "cloned"}, cloneErr, false, tt.init, tt.init)
			if cloneErr != nil {
				assert.Nil(t, cloned, "failed clone must not be exposed as a completed delivery")
			} else {
				require.NotNil(t, cloned)
				if tt.init {
					assert.Equal(t, "test-traffic-token", cloned.GetTrafficAccessToken())
				}
			}
		})
	}
}

func TestResumeDeferredClaim(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("delivery-fails=%t", fail), func(t *testing.T) {
			const stage = "/mnt/envd/claim/.startup-TEST000000"
			processServer := testutils.NewTestRuntimeServer(testutils.TestRuntimeServerOptions{
				RunCommandImmediately: true,
				RunCommandResult:      runtime.RunCommandResult{Exited: true, Stdout: []string{stage + "\n"}},
			})
			defer processServer.Close()
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/files" {
					processServer.Config.Handler.ServeHTTP(w, r)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				calls++
				assert.Equal(t, stage+"/.env_ready", r.URL.Query().Get("path"))
				file, _, err := r.FormFile("file")
				if assert.NoError(t, err) {
					defer file.Close()
					data, err := io.ReadAll(file)
					assert.NoError(t, err)
					assert.Equal(t, "current-pod-uid", string(data), "delivery must use the refreshed Pod UID")
				}
				if fail {
					http.Error(w, "failed", http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			record, err := json.Marshal(runtimeconfig.InitRuntimeOptions{SkipInit: true})
			require.NoError(t, err)
			box := &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "claimed", Namespace: "default", UID: "sandbox-uid", Generation: 2, Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeURL: server.URL, v1alpha1.AnnotationInitRuntimeRequest: string(record),
				v1alpha1.AnnotationRuntimeAccessToken: runtime.AccessToken,
			}, Labels: map[string]string{v1alpha1.LabelSandboxIsClaimed: v1alpha1.True}}, Spec: v1alpha1.SandboxSpec{StartupMode: v1alpha1.StartupModeDeferred},
				Status: v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning, ObservedGeneration: 2,
					PodInfo: v1alpha1.PodInfo{PodUID: "old-pod-uid", PodIP: "10.0.0.1"}, Conditions: []metav1.Condition{
						{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "StartupDeliveryPending"},
						{Type: string(v1alpha1.RuntimeInitialized), Status: metav1.ConditionFalse, Reason: "StartupDeliveryPending"},
					}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: box.Name, Namespace: box.Namespace, UID: "current-pod-uid",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, v1alpha1.GroupVersion.WithKind("Sandbox"))}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "127.0.0.1",
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			cache, cli, err := cachetest.NewTestCache(t, box, pod)
			require.NoError(t, err)
			stale := box.DeepCopy()
			stale.Generation = 1
			stale.Status.ObservedGeneration = 1
			err = claimengine.ResumeDeferredClaim(t.Context(), stale, infra.ClaimSandboxOptions{WaitReadyTimeout: time.Second}, cache)
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
				assert.Equal(t, "current-pod-uid", request.StartupPodUID)
				require.NoError(t, claimengine.ResumeDeferredClaim(t.Context(), stored, infra.ClaimSandboxOptions{}, cache))
			}
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, 1, calls)
		})
	}
}

func TestWaitForSandboxReadyDeferredStartup(t *testing.T) {
	for _, tt := range []struct {
		name       string
		mutate     func(*v1alpha1.Sandbox, *corev1.Pod)
		wantErr    bool
		snapshotRV string
	}{
		{name: "owned native Ready bypasses gated Sandbox Ready"},
		{name: "pre-claim resource version waits", wantErr: true, snapshotRV: "2", mutate: func(box *v1alpha1.Sandbox, _ *corev1.Pod) { box.ResourceVersion = "1" }},
		{name: "older generation waits", wantErr: true, mutate: func(box *v1alpha1.Sandbox, _ *corev1.Pod) { box.Generation = 0; box.Status.ObservedGeneration = 0 }},
		{name: "unclaimed stock cannot bypass", wantErr: true, mutate: func(box *v1alpha1.Sandbox, _ *corev1.Pod) { box.Labels = nil }},
		{name: "Immediate cannot bypass", wantErr: true, mutate: func(box *v1alpha1.Sandbox, _ *corev1.Pod) { box.Spec.StartupMode = v1alpha1.StartupModeImmediate }},
		{name: "Pod without IP waits", wantErr: true, mutate: func(_ *v1alpha1.Sandbox, pod *corev1.Pod) { pod.Status.PodIP = "" }},
		{name: "upgrade initialization also avoids circular wait", mutate: func(box *v1alpha1.Sandbox, _ *corev1.Pod) { box.Status.Phase = v1alpha1.SandboxUpgrading }},
		{name: "unobserved generation waits", wantErr: true, mutate: func(box *v1alpha1.Sandbox, _ *corev1.Pod) { box.Status.ObservedGeneration-- }},
		{name: "successor Sandbox UID rejected", wantErr: true, mutate: func(box *v1alpha1.Sandbox, _ *corev1.Pod) { box.UID = "successor" }},
		{name: "unowned Pod rejected", wantErr: true, mutate: func(_ *v1alpha1.Sandbox, pod *corev1.Pod) { pod.OwnerReferences = nil }},
		{name: "previous Sandbox owner rejected", wantErr: true, mutate: func(_ *v1alpha1.Sandbox, pod *corev1.Pod) { pod.OwnerReferences[0].UID = "old-sandbox" }},
		{name: "foreign controller rejected", wantErr: true, mutate: func(_ *v1alpha1.Sandbox, pod *corev1.Pod) { pod.OwnerReferences[0].Kind = "Other" }},
		{name: "Pod not Ready waits", wantErr: true, mutate: func(_ *v1alpha1.Sandbox, pod *corev1.Pod) { pod.Status.Conditions[0].Status = corev1.ConditionFalse }},
		{name: "terminating Pod waits", wantErr: true, mutate: func(_ *v1alpha1.Sandbox, pod *corev1.Pod) {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
			pod.Finalizers = []string{"test"}
		}},
		{name: "empty Pod UID waits", wantErr: true, mutate: func(_ *v1alpha1.Sandbox, pod *corev1.Pod) { pod.UID = "" }},
		{name: "paused Sandbox waits", wantErr: true, mutate: func(box *v1alpha1.Sandbox, _ *corev1.Pod) { box.Spec.Paused = true }},
		{name: "inplace update waits", wantErr: true, mutate: func(box *v1alpha1.Sandbox, _ *corev1.Pod) {
			box.Status.Conditions = append(box.Status.Conditions, metav1.Condition{Type: string(v1alpha1.SandboxConditionInplaceUpdate), Reason: v1alpha1.SandboxInplaceUpdateReasonInplaceUpdating})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			box := &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "claimed", Namespace: "default", UID: "sandbox-uid", Generation: 2,
				Labels:      map[string]string{v1alpha1.LabelSandboxIsClaimed: v1alpha1.True},
				Annotations: map[string]string{v1alpha1.AnnotationInitRuntimeRequest: `{"skipInit":true}`}},
				Spec: v1alpha1.SandboxSpec{StartupMode: v1alpha1.StartupModeDeferred},
				Status: v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning, ObservedGeneration: 2,
					PodInfo: v1alpha1.PodInfo{PodUID: "stale-pod", PodIP: "10.0.0.1"}, Conditions: []metav1.Condition{
						{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse, Reason: "StartupDeliveryPending"},
						{Type: string(v1alpha1.RuntimeInitialized), Status: metav1.ConditionFalse, Reason: "StartupDeliveryPending"},
					}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: box.Name, Namespace: box.Namespace, UID: "current-pod",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, v1alpha1.GroupVersion.WithKind("Sandbox"))}},
				Spec: corev1.PodSpec{NodeName: "current-node"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.2",
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			stale := box.DeepCopy()
			stale.Generation = 1
			stale.Status.ObservedGeneration = 1
			if tt.snapshotRV != "" {
				stale.ResourceVersion = tt.snapshotRV
			}
			if tt.mutate != nil {
				tt.mutate(box, pod)
			}
			cache, _, err := cachetest.NewTestCache(t, box, pod)
			require.NoError(t, err)
			sbx := AsSandbox(stale, cache)
			_, creation, err := waitForSandboxReady(t.Context(), sbx, infra.ClaimSandboxOptions{}, cache, true)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Nil(t, creation, "native Pod readiness must not record application readiness")
			assert.Equal(t, int64(2), sbx.Generation)
			assert.Equal(t, sbx.Generation, sbx.Status.ObservedGeneration)
			assert.Equal(t, pod.UID, sbx.Status.PodInfo.PodUID)
			assert.Equal(t, pod.Status.PodIP, sbx.Status.PodInfo.PodIP)
			assert.Equal(t, pod.Spec.NodeName, sbx.Status.PodInfo.NodeName)
			assert.Equal(t, metav1.ConditionFalse, GetSandboxCondition(sbx.Sandbox, v1alpha1.SandboxConditionReady).Status)
			assert.Equal(t, "stale-pod", string(stale.Status.PodInfo.PodUID), "input snapshots remain immutable")
		})
	}
}

func TestClaimStartupReleaseOrdering(t *testing.T) {
	for _, tt := range []struct {
		name, fault string
		mode        v1alpha1.StartupMode
		skipInit    bool
		reserveFor  *time.Duration
	}{
		{name: "init before signal", mode: v1alpha1.StartupModeDeferred},
		{name: "skipped init still signals", mode: v1alpha1.StartupModeDeferred, skipInit: true},
		{name: "Immediate does not signal"},
		{name: "delivery failure returned and cleaned", mode: v1alpha1.StartupModeDeferred, fault: "env.sh"},
		{name: "commit response lost recovers exact Pod UID", mode: v1alpha1.StartupModeDeferred, fault: "commit-lost"},
		{name: "terminated commit retries publication without repeating init or mounts", mode: v1alpha1.StartupModeDeferred, fault: "commit-failed-once"},
		{name: "terminated unpublished commit exhausts retries and cleans claim", mode: v1alpha1.StartupModeDeferred, fault: "commit-failed"},
		{name: "lock contention preserves an unpublished claim", mode: v1alpha1.StartupModeDeferred, fault: "commit-busy"},
		{name: "nonzero exit after publication only records completion", mode: v1alpha1.StartupModeDeferred, fault: "commit-exit-after-publication"},
		{name: "successful terminal event survives a lost trailer", mode: v1alpha1.StartupModeDeferred, fault: "commit-zero-lost"},
		{name: "zero exit with End error and absent signal remains unknown", mode: v1alpha1.StartupModeDeferred, fault: "commit-process-error-absent"},
		{name: "zero exit with End error and denied read remains unknown", mode: v1alpha1.StartupModeDeferred, fault: "commit-process-error-denied"},
		{name: "zero exit with End error requires a matching publication signal", mode: v1alpha1.StartupModeDeferred, fault: "commit-process-error-published"},
		{name: "skipped init response lost recovers", mode: v1alpha1.StartupModeDeferred, skipInit: true, fault: "commit-lost"},
		{name: "cleanup failure recovers publication", mode: v1alpha1.StartupModeDeferred, fault: "cleanup-lost"},
		{name: "commit uncertainty with denied read preserves claim", mode: v1alpha1.StartupModeDeferred, fault: "read-denied"},
		{name: "commit uncertainty with stale UID preserves claim", mode: v1alpha1.StartupModeDeferred, fault: "read-mismatch"},
		{name: "commit uncertainty with absent signal preserves claim", mode: v1alpha1.StartupModeDeferred, fault: "read-absent"},
		{name: "uncertainty skips delayed shutdown", mode: v1alpha1.StartupModeDeferred, fault: "read-denied", reserveFor: ptr.To(time.Hour)},
		{name: "precommit command failure cleans claim", mode: v1alpha1.StartupModeDeferred, fault: "cmd.sh"},
		{name: "precommit signal failure cleans claim", mode: v1alpha1.StartupModeDeferred, fault: ".env_ready"},
		{name: "completion bookkeeping failure preserves published claim", mode: v1alpha1.StartupModeDeferred, fault: "record-denied"},
		{name: "recovered publication survives bookkeeping failure", mode: v1alpha1.StartupModeDeferred, fault: "commit-lost-record-denied"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := runtimeconfig.StartupData{}
			if tt.mode == v1alpha1.StartupModeDeferred {
				data = runtimeconfig.StartupData{EnvVars: map[string]string{"VALUE": "claim"}, Command: []string{"printf", "%s", "claim-command"}}
			}
			rt := newStartupPublicationRuntime(t, tt.fault, "pod-uid", data)
			box := &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Name: "claimed", Namespace: "default", UID: types.UID(uuid.NewString()), CreationTimestamp: metav1.Now(),
				Labels: map[string]string{v1alpha1.LabelSandboxTemplate: "test-template"}, OwnerReferences: GetSbsOwnerReference(),
				Annotations: map[string]string{v1alpha1.AnnotationRuntimeURL: rt.server.URL, v1alpha1.AnnotationRuntimeAccessToken: runtime.AccessToken},
			}, Spec: v1alpha1.SandboxSpec{StartupMode: tt.mode,
				EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "app:test"}}}}}},
				Status: v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning,
					PodInfo:    v1alpha1.PodInfo{PodUID: "pod-uid", PodIP: "127.0.0.1"},
					Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}}}
			opts := infra.ClaimSandboxOptions{User: "owner", Template: "test-template", Namespace: "default",
				AccessToken: runtime.AccessToken, ReserveFailedSandboxFor: tt.reserveFor,
				InitRuntime: &runtimeconfig.InitRuntimeOptions{EnvVars: data.EnvVars, Command: data.Command, AccessToken: runtime.AccessToken, SkipInit: true}}
			if !tt.skipInit {
				box.Annotations[identity.AnnotationAgentName] = "test-agent"
				box.Annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
				opts.InitRuntime = &runtimeconfig.InitRuntimeOptions{AccessToken: runtime.AccessToken, SkipRefresh: true, EnvVars: data.EnvVars, Command: data.Command}
				opts.CSIMount = rt.withIdentityAndStorage()
			}
			opts, err := claimengine.ValidateAndInitClaimOptions(opts)
			require.NoError(t, err)
			cache, cli, err := cachetest.NewTestCache(t, box, startupPublicationPod(box))
			require.NoError(t, err)
			provider := rt.withCompletionChecks(cache, cli)
			var pickCache sync.Map
			workers := make(chan struct{}, 1)
			claimed, metrics, claimErr := claimengine.TryClaimSandbox(t.Context(), opts, &pickCache, provider, workers, nil)
			require.NotNil(t, claimed, "the actual claim must reach the locked sandbox")
			assert.Equal(t, infra.LockTypeUpdate, metrics.LockType)
			assert.Equal(t, claimErr, metrics.LastError)
			assert.Empty(t, workers)
			_, picked := pickCache.Load(client.ObjectKeyFromObject(box).String())
			assert.False(t, picked)
			rt.assertDeliveryResult(cli, client.ObjectKeyFromObject(box), claimErr, true, !tt.skipInit, !tt.skipInit)
			if !tt.skipInit {
				assert.Equal(t, "test-traffic-token", claimed.TrafficToken().AccessToken)
			}
		})
	}
}

const startupPublicationStage = "/mnt/envd/claim/.startup-TEST000000"

// The static runtime fixture cannot model a commit that publishes before losing
// its terminal response. This local handler keeps only the state needed to drive
// the real runtime protocol and claim/clone cleanup paths; it executes no shell.
type startupPublicationRuntime struct {
	processconnect.UnimplementedProcessHandler
	t         *testing.T
	server    *httptest.Server
	fault     string
	uid       string
	data      runtimeconfig.StartupData
	mu        sync.Mutex
	calls     []string
	files     map[string]string
	published int
	commits   int
	initToken string
}

func newStartupPublicationRuntime(t *testing.T, fault, uid string, data runtimeconfig.StartupData) *startupPublicationRuntime {
	t.Helper()
	rt := &startupPublicationRuntime{t: t, fault: fault, uid: uid, data: data, files: map[string]string{}}
	mux := http.NewServeMux()
	path, handler := processconnect.NewProcessHandler(rt)
	mux.Handle(path, handler)
	mux.HandleFunc("/files", rt.handleFiles)
	mux.HandleFunc("/init", func(w http.ResponseWriter, r *http.Request) {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		rt.calls = append(rt.calls, "init")
		assert.Equal(t, http.MethodPost, r.Method)
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); !assert.NoError(t, err) {
			http.Error(w, "invalid init", http.StatusBadRequest)
			return
		}
		assert.NotContains(t, body, "command")
		assert.NotContains(t, body, "startupDelivered")
		assert.NotContains(t, body, "startupPodUID")
		assert.NotContains(t, body, "skipInit")
		assert.NoError(t, json.Unmarshal(body["accessToken"], &rt.initToken))
		assert.NotEmpty(t, rt.initToken)
		if len(rt.data.EnvVars) > 0 {
			var env map[string]string
			assert.NoError(t, json.Unmarshal(body["envVars"], &env))
			assert.Equal(t, rt.data.EnvVars, env)
		}
		assert.Zero(t, rt.published)
		w.WriteHeader(http.StatusNoContent)
	})
	rt.server = httptest.NewServer(mux)
	t.Cleanup(rt.server.Close)
	return rt
}

func (rt *startupPublicationRuntime) checkAuth(header http.Header) {
	assert.Equal(rt.t, "Basic cm9vdDo=", header.Get("Authorization"))
	// Skipped-init clones are unbound and use a freshly generated token. Once
	// /init binds a token, all native calls must use that exact token.
	assert.NotEmpty(rt.t, header.Get("X-Access-Token"))
	if rt.initToken != "" {
		assert.Equal(rt.t, rt.initToken, header.Get("X-Access-Token"))
	}
}

func (rt *startupPublicationRuntime) Start(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.checkAuth(req.Header())
	cfg := req.Msg.GetProcess()
	if !assert.NotNil(rt.t, cfg) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("missing process"))
	}
	phase, stdout := "", ""
	var exitCode int32
	if cfg.Cmd == runtime.MountCommand {
		phase = "mount"
		if !assert.Len(rt.t, cfg.Args, 5) {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid mount arguments"))
		}
		assert.Equal(rt.t, []string{"mount", "--driver", "test.csi", "--config"}, cfg.Args[:4])
		raw, err := base64.StdEncoding.DecodeString(cfg.Args[4])
		assert.NoError(rt.t, err)
		var request csi.NodePublishVolumeRequest
		assert.NoError(rt.t, proto.Unmarshal(raw, protoadapt.MessageV2Of(&request)))
		assert.Equal(rt.t, "test-volume", request.VolumeId)
		assert.Equal(rt.t, "/data", request.TargetPath)
		assert.Equal(rt.t, rt.uid, cfg.Envs["POD_UID"])
		assert.Zero(rt.t, rt.published)
	} else {
		assert.Equal(rt.t, "bash", cfg.Cmd)
		if len(cfg.Args) < 5 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid startup arguments"))
		}
		assert.Equal(rt.t, "-c", cfg.Args[0])
		assert.Equal(rt.t, "startup-delivery", cfg.Args[2])
		assert.Equal(rt.t, "/mnt/envd/claim", cfg.Args[3])
		assert.Equal(rt.t, "5000", req.Header().Get("Connect-Timeout-Ms"))
		assert.Empty(rt.t, cfg.Envs, "startup payload belongs only in native file uploads")
		assert.NotContains(rt.t, strings.Join(cfg.Args, " "), "export VALUE=")
		assert.NotContains(rt.t, strings.Join(cfg.Args, " "), rt.data.Command[len(rt.data.Command)-1])
		switch {
		case strings.Contains(cfg.Args[1], "mktemp -d"):
			phase, stdout = "prepare", startupPublicationStage+"\n"
			assert.Equal(rt.t, []string{"/mnt/envd/claim", rt.uid}, cfg.Args[3:])
		case strings.Contains(cfg.Args[1], "flock -n"):
			phase = "commit"
			assert.Equal(rt.t, []string{"/mnt/envd/claim", rt.uid, startupPublicationStage, "1", "1"}, cfg.Args[3:])
			assert.Equal(rt.t, rt.uid, rt.files[startupPublicationStage+"/.env_ready"])
			assert.Len(rt.t, rt.files, 3, "commit cannot run until all staged data exists")
			rt.commits++
			switch {
			case rt.fault == "commit-busy":
				exitCode = 75
			case strings.HasPrefix(rt.fault, "commit-process-error"):
				if rt.fault == "commit-process-error-published" {
					rt.published++
				}
			case rt.fault == "commit-failed" || (rt.fault == "commit-failed-once" && rt.commits == 1):
				exitCode = 42
			default:
				rt.published++
				if rt.fault == "commit-exit-after-publication" {
					exitCode = 42
				}
			}
		case strings.Contains(cfg.Args[1], "rm -rf"):
			phase = "cleanup"
			assert.Equal(rt.t, []string{"/mnt/envd/claim", startupPublicationStage}, cfg.Args[3:])
		default:
			return connect.NewError(connect.CodeInvalidArgument, errors.New("unexpected startup phase"))
		}
	}
	rt.calls = append(rt.calls, phase)
	if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 1}}}}); err != nil {
		return err
	}
	if (phase == "commit" && (strings.HasPrefix(rt.fault, "commit-lost") || strings.HasPrefix(rt.fault, "read-"))) || (phase == "cleanup" && rt.fault == "cleanup-lost") {
		// No End event: the app has already started, but the client receives an
		// unavailable stream trailer rather than a successful terminal response.
		return connect.NewError(connect.CodeUnavailable, errors.New("test response lost"))
	}
	if stdout != "" {
		if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(stdout)}}}}}); err != nil {
			return err
		}
	}
	var processError *string
	if exitCode != 0 {
		processError = ptr.To(fmt.Sprintf("exit status %d", exitCode))
	} else if phase == "commit" && strings.HasPrefix(rt.fault, "commit-process-error") {
		processError = ptr.To("test process result error")
	}
	if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true, ExitCode: exitCode, Error: processError}}}}); err != nil {
		return err
	}
	if phase == "commit" && rt.fault == "commit-zero-lost" {
		return connect.NewError(connect.CodeUnavailable, errors.New("test trailer lost after successful End"))
	}
	return nil
}

func (rt *startupPublicationRuntime) handleFiles(w http.ResponseWriter, r *http.Request) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.checkAuth(r.Header)
	assert.Equal(rt.t, "root", r.URL.Query().Get("username"))
	path := r.URL.Query().Get("path")
	if r.Method == http.MethodGet {
		rt.calls = append(rt.calls, "verify")
		assert.Equal(rt.t, "/mnt/envd/claim/.env_ready", path)
		if rt.published == 0 || rt.fault == "read-absent" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if rt.fault == "read-denied" || rt.fault == "commit-process-error-denied" {
			http.Error(w, "test read denied", http.StatusForbidden)
			return
		}
		uid := rt.uid
		if rt.fault == "read-mismatch" {
			uid = "source-pod-uid"
		}
		_, err := io.WriteString(w, uid)
		assert.NoError(rt.t, err)
		return
	}
	assert.Equal(rt.t, http.MethodPost, r.Method)
	assert.Zero(rt.t, rt.published, "no uploads may follow publication")
	assert.Contains(rt.t, rt.calls, "prepare")
	name := strings.TrimPrefix(path, startupPublicationStage+"/")
	rt.calls = append(rt.calls, "write:"+name)
	file, _, err := r.FormFile("file")
	if !assert.NoError(rt.t, err) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer func() { assert.NoError(rt.t, file.Close()) }()
	body, err := io.ReadAll(file)
	assert.NoError(rt.t, err)
	var expected string
	switch name {
	case "env.sh":
		for name, value := range rt.data.EnvVars {
			expected += "export " + name + "='" + value + "'\n"
		}
	case "cmd.sh":
		expected = "exec -- '" + strings.Join(rt.data.Command, "' '") + "'\n"
	case ".env_ready":
		expected = rt.uid
	default:
		rt.t.Errorf("unexpected file upload path %q", path)
	}
	assert.Equal(rt.t, expected, string(body))
	if rt.fault == name {
		http.Error(w, "test staged write denied", http.StatusForbidden)
		return
	}
	rt.files[path] = string(body)
	w.WriteHeader(http.StatusNoContent)
}

func (rt *startupPublicationRuntime) withIdentityAndStorage() *runtimeconfig.CSIMountOptions {
	rt.t.Helper()
	identity.RegisterProvider(&mockIdentityProvider{
		issueTokenWithKindFunc: func(_ context.Context, _ *v1alpha1.Sandbox, kind identity.TokenKind) (*identity.TokenResponse, error) {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			phase, token := "id-issue", "test-id-token"
			if kind == identity.TokenKindAccessToken {
				phase, token = "access-issue", "test-traffic-token"
			} else {
				assert.Equal(rt.t, identity.TokenKindIDToken, kind)
			}
			rt.calls = append(rt.calls, phase)
			assert.Zero(rt.t, rt.published)
			return &identity.TokenResponse{AccessToken: token, AccessTokenExpiration: time.Now().Add(time.Hour).Format(time.RFC3339)}, nil
		},
		propagateFunc: func(_ context.Context, _ *v1alpha1.Sandbox, response *identity.TokenResponse) error {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			rt.calls = append(rt.calls, "propagate")
			assert.Equal(rt.t, "test-id-token", response.AccessToken)
			assert.Zero(rt.t, rt.published)
			return nil
		},
	})
	rt.t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
	return &runtimeconfig.CSIMountOptions{Concurrency: 1, MountOptionList: []runtimeconfig.MountConfig{{
		Driver: "test.csi", PublishRequest: &csi.NodePublishVolumeRequest{VolumeId: "test-volume", TargetPath: "/data"},
	}}}
}

func (rt *startupPublicationRuntime) withCompletionChecks(cache infracache.Provider, cli client.Client) infracache.Provider {
	rt.t.Helper()
	wrapped := interceptor.NewClient(cli.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			box, ok := obj.(*v1alpha1.Sandbox)
			if ok {
				rt.mu.Lock()
				defer rt.mu.Unlock()
				request, err := runtime.GetInitRuntimeRequest(box)
				if err != nil {
					return err
				}
				if request != nil && request.StartupDelivered {
					rt.calls = append(rt.calls, "record")
					assert.Equal(rt.t, 1, rt.published, "completion must follow exactly one publication")
					assert.Equal(rt.t, rt.uid, request.StartupPodUID)
					if strings.Contains(rt.fault, "record-denied") {
						return apierrors.NewForbidden(v1alpha1.Resource("sandboxes"), box.Name, errors.New("test completion denied"))
					}
				} else if box.Annotations[identity.AgentKeyTokenRefreshStatus] != "" {
					rt.calls = append(rt.calls, "token-record")
					assert.Zero(rt.t, rt.published)
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*v1alpha1.Sandbox); ok {
				rt.mu.Lock()
				rt.calls = append(rt.calls, "delete")
				rt.mu.Unlock()
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	return &apiReaderOverrideCache{Provider: cache, client: wrapped}
}

func startupPublicationPod(box *v1alpha1.Sandbox) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: box.Name, Namespace: box.Namespace, UID: box.Status.PodInfo.PodUID,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, v1alpha1.GroupVersion.WithKind("Sandbox"))},
	}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "127.0.0.1", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
}

func (rt *startupPublicationRuntime) assertDeliveryResult(cli client.Client, key client.ObjectKey, deliveryErr error, claim, init, identityAndStorage bool) {
	rt.t.Helper()
	t := rt.t
	rt.mu.Lock()
	defer rt.mu.Unlock()
	precommit := rt.fault == "env.sh" || rt.fault == "cmd.sh" || rt.fault == ".env_ready"
	failed := precommit || rt.fault == "commit-failed"
	uncertain := strings.HasPrefix(rt.fault, "read-") || rt.fault == "commit-busy" ||
		(strings.HasPrefix(rt.fault, "commit-process-error") && rt.fault != "commit-process-error-published")
	recordDenied := strings.Contains(rt.fault, "record-denied")
	deferred := len(rt.data.Command) > 0
	if failed || uncertain || recordDenied {
		require.Error(t, deliveryErr)
		var retryErr claimengine.RetriableError
		assert.Equal(t, claim && failed, errors.As(deliveryErr, &retryErr), "only a confirmed prepublication claim failure may retry")
		assert.Equal(t, uncertain, errors.Is(deliveryErr, runtime.ErrStartupPublicationUncertain))
		if strings.HasPrefix(rt.fault, "read-") {
			var streamErr *connect.Error
			require.ErrorAs(t, deliveryErr, &streamErr)
			assert.Equal(t, connect.CodeUnavailable, streamErr.Code())
		}
		if recordDenied {
			assert.True(t, apierrors.IsForbidden(deliveryErr), "completion error remains terminal and classified")
		}
	} else {
		require.NoError(t, deliveryErr)
	}
	expected := []string{}
	if init {
		expected = append(expected, "init")
	}
	if identityAndStorage {
		expected = append(expected, "id-issue", "propagate", "token-record", "access-issue", "mount")
	}
	if deferred {
		attempts := 1
		if rt.fault == "commit-failed-once" {
			attempts = 2
		} else if rt.fault == "commit-failed" {
			attempts = 3
		}
		for attempt := 0; attempt < attempts; attempt++ {
			expected = append(expected, "prepare")
			for _, name := range []string{"env.sh", "cmd.sh", ".env_ready"} {
				expected = append(expected, "write:"+name)
				if name == rt.fault {
					break
				}
			}
			if !precommit {
				expected = append(expected, "commit")
			}
			lostEnd := strings.HasPrefix(rt.fault, "read-") || strings.HasPrefix(rt.fault, "commit-lost")
			processError := strings.HasPrefix(rt.fault, "commit-process-error")
			if !lostEnd && !processError {
				expected = append(expected, "cleanup")
			}
			if lostEnd || processError || rt.fault == "commit-busy" || rt.fault == "commit-failed" ||
				(rt.fault == "commit-failed-once" && attempt == 0) || rt.fault == "commit-exit-after-publication" {
				expected = append(expected, "verify")
			}
		}
		if !failed && !uncertain {
			expected = append(expected, "record")
		}
	}
	if failed {
		expected = append(expected, "delete")
	}
	assert.Equal(t, expected, rt.calls, "publication retries must not repeat initialization, tokens or mounts")
	wantPublications := 0
	if deferred && !failed && rt.fault != "commit-busy" &&
		(!strings.HasPrefix(rt.fault, "commit-process-error") || rt.fault == "commit-process-error-published") {
		wantPublications = 1
	}
	assert.Equal(t, wantPublications, rt.published)
	stored := &v1alpha1.Sandbox{}
	err := cli.Get(t.Context(), key, stored)
	if failed {
		assert.True(t, apierrors.IsNotFound(err), "confirmed unpublished failure must retain default deletion behavior: %v", err)
		return
	}
	require.NoError(t, err, "published or uncertain apps must not be deleted")
	assert.True(t, stored.DeletionTimestamp.IsZero())
	assert.Nil(t, stored.Spec.ShutdownTime, "cleanup must not schedule shutdown of a possibly started app")
	assert.NotEqual(t, v1alpha1.True, stored.Labels[v1alpha1.LabelSandboxReservedFailed])
	assert.Equal(t, v1alpha1.True, stored.Labels[v1alpha1.LabelSandboxIsClaimed])
	assert.Equal(t, "owner", stored.Annotations[v1alpha1.AnnotationOwner])
	assert.NotEmpty(t, stored.Annotations[v1alpha1.AnnotationLock])
	assert.Empty(t, stored.OwnerReferences, "claimed deliveries no longer belong to the pool")
	request, err := runtime.GetInitRuntimeRequest(stored)
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, rt.data.EnvVars, request.EnvVars)
	assert.Equal(t, rt.data.Command, request.Command)
	assert.Equal(t, deferred && deliveryErr == nil, request.StartupDelivered)
	if request.StartupDelivered {
		assert.Equal(t, rt.uid, request.StartupPodUID, "completion binds the exact current Pod UID, never the checkpoint UID")
	} else {
		assert.Empty(t, request.StartupPodUID)
	}
}
