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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

type buildProcessHandler struct {
	processconnect.UnimplementedProcessHandler
	start func(context.Context, *connect.Request[process.StartRequest], *connect.ServerStream[process.StartResponse]) error
}

func (h *buildProcessHandler) Start(ctx context.Context, request *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	return h.start(ctx, request, stream)
}

func executionFixture(t *testing.T) (*Reconciler, *api.SandboxTemplate, *api.Sandbox, *corev1.Pod) {
	t.Helper()
	version := modelFixtures()
	version.Spec.Template.Spec.Containers[0].Image = "example.com/runtime@sha256:" + strings.Repeat("a", 64)
	version.Spec.Checkpoint = &api.CheckpointStrategy{Enable: true}
	version.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Steps: []api.TemplateBuildStep{
		{Command: &api.TemplateBuildCommand{Command: "first", User: "alice", Cwd: "/app", Env: map[string]string{"BUILD_ENV": "value"}}},
		{Command: &api.TemplateBuildCommand{Command: "second"}},
	}}
	version.Spec.PersistentContents = []string{api.PersistentContentFilesystem}
	box := &api.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: version.Namespace, Name: version.Name, UID: "sandbox-uid", Generation: 1, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: version.UID, Controller: ptr.To(true)}}},
		Spec:       api.SandboxSpec{EmbeddedSandboxTemplate: api.EmbeddedSandboxTemplate{Template: version.Spec.Template.DeepCopy()}, Runtimes: version.Spec.Runtimes, PersistentContents: version.Spec.PersistentContents},
		Status:     api.SandboxStatus{Phase: api.SandboxRunning, ObservedGeneration: 1, PodInfo: api.PodInfo{PodIP: "127.0.0.1", PodUID: "pod-uid"}, Conditions: []metav1.Condition{{Type: string(api.SandboxConditionReady), Status: metav1.ConditionTrue}}},
	}
	data, err := json.Marshal(*box.Spec.Template)
	require.NoError(t, err)
	box.Status.UpdateRevision = utils.HashData(data)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: version.Namespace, Name: version.Name, UID: "pod-uid", Labels: map[string]string{api.PodLabelTemplateHash: box.Status.UpdateRevision}, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Sandbox", Name: box.Name, UID: box.UID, Controller: ptr.To(true)}}},
		Spec:       box.Spec.Template.Spec,
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "runtime", Image: version.Spec.Template.Spec.Containers[0].Image, ImageID: "sha256:" + strings.Repeat("a", 64), ContainerID: "containerd://original", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	}
	data, err = json.Marshal(executionIdentity{SandboxUID: box.UID, PodUID: pod.UID})
	require.NoError(t, err)
	version.Annotations = map[string]string{executionIdentityAnnotation: string(data)}
	return modelReconciler(t, version, box, pod), version, box, pod
}

func startExecutionManager(t *testing.T, r *Reconciler) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	require.Eventually(t, func() bool {
		r.workerMu.Lock()
		defer r.workerMu.Unlock()
		return r.workerCtx != nil
	}, 3*time.Second, time.Millisecond)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(6 * time.Second):
			t.Error("execution manager did not drain workers")
		}
	})
	return cancel
}

func bindBuildRuntime(t *testing.T, r *Reconciler, box *api.Sandbox, handler *buildProcessHandler) {
	t.Helper()
	mux := http.NewServeMux()
	path, processHandler := processconnect.NewProcessHandler(handler)
	mux.Handle(path, processHandler)
	mux.HandleFunc("/init", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	current := &api.Sandbox{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(box), current))
	current.Annotations = map[string]string{api.AnnotationRuntimeURL: server.URL}
	require.NoError(t, r.Update(t.Context(), current))
}

func waitExecutionCondition(t *testing.T, r *Reconciler, version *api.SandboxTemplate, reason string) *api.SandboxTemplate {
	t.Helper()
	current := &api.SandboxTemplate{}
	require.Eventually(t, func() bool {
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(version), current); err != nil {
			return false
		}
		condition := meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition)
		return condition != nil && condition.Reason == reason && !r.hasWorker(version.UID)
	}, 5*time.Second, time.Millisecond)
	return current
}

// mutateStoredVersion applies mutate to the stored Version and writes it back.
func mutateStoredVersion(t *testing.T, r *Reconciler, version *api.SandboxTemplate, mutate func(*api.SandboxTemplate)) {
	t.Helper()
	current := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
	mutate(current)
	require.NoError(t, r.Update(t.Context(), current))
}

// mutateStoredBox applies mutate to the stored Sandbox and writes it back.
func mutateStoredBox(t *testing.T, r *Reconciler, box *api.Sandbox, mutate func(*api.Sandbox)) {
	t.Helper()
	current := &api.Sandbox{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(box), current))
	mutate(current)
	require.NoError(t, r.Update(t.Context(), current))
}

// removeStoredVersion deletes the stored Version.
func removeStoredVersion(t *testing.T, r *Reconciler, version *api.SandboxTemplate) {
	t.Helper()
	require.NoError(t, r.Delete(t.Context(), version))
}

// setStoredIdentity overwrites the execution identity annotation on the stored Version.
func setStoredIdentity(t *testing.T, r *Reconciler, version *api.SandboxTemplate, value string) {
	t.Helper()
	mutateStoredVersion(t, r, version, func(v *api.SandboxTemplate) { v.Annotations[executionIdentityAnnotation] = value })
}

// failGetFor makes reads of proto's type return failure.
func failGetFor(proto client.Object, failure error) interceptor.Funcs {
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if reflect.TypeOf(obj) == reflect.TypeOf(proto) {
			return failure
		}
		return c.Get(ctx, key, obj, opts...)
	}}
}

// failUpdateFor makes writes of proto's type return failure.
func failUpdateFor(proto client.Object, failure error) interceptor.Funcs {
	return interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if reflect.TypeOf(obj) == reflect.TypeOf(proto) {
			return failure
		}
		return c.Update(ctx, obj, opts...)
	}}
}

func TestBuildExecutionAsyncSerialCaptureAndCleanup(t *testing.T) {
	r, version, box, _ := executionFixture(t)
	version.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	require.NoError(t, r.Update(t.Context(), version))
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	bindBuildRuntime(t, r, box, &buildProcessHandler{start: func(ctx context.Context, request *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		if calls.Add(1) == 1 {
			require.Equal(t, []string{"-l", "-c", "first"}, request.Msg.Process.Args)
			require.Equal(t, "/app", request.Msg.Process.GetCwd())
			require.Equal(t, "value", request.Msg.Process.Envs["BUILD_ENV"])
			require.NotEmpty(t, request.Header().Get("Connect-Timeout-Ms"))
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			require.Equal(t, []string{"-l", "-c", "second"}, request.Msg.Process.Args)
		}
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true}}}})
	}})
	startExecutionManager(t, r)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)}
	returned := make(chan error, 1)
	go func() { _, err := r.ReconcileVersion(t.Context(), req); returned <- err }()
	select {
	case err := <-returned:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("reconcile blocked on the build command")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first build command did not start")
	}
	_, err := r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
	metadata := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, metadata))
	running := meta.FindStatusCondition(metadata.Status.Conditions, buildReadyCondition)
	require.NotNil(t, running)
	require.Equal(t, metav1.ConditionFalse, running.Status)
	buildStarted := running.LastTransitionTime
	require.True(t, buildStarted.After(metadata.CreationTimestamp.Add(r.Options.Timeout)))
	metadata.Labels["editor"] = "metadata"
	require.NoError(t, r.Update(t.Context(), metadata))
	close(release)
	provisioned := waitExecutionCondition(t, r, version, buildReasonProvisioned)
	require.Len(t, provisioned.Status.Conditions, 1)
	condition := meta.FindStatusCondition(provisioned.Status.Conditions, buildReadyCondition)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.True(t, buildStarted.Time.Equal(condition.LastTransitionTime.Time))
	require.EqualValues(t, 2, calls.Load())
	_, err = r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	checkpoint := &api.Checkpoint{}
	key := client.ObjectKey{Namespace: version.Namespace, Name: version.Name + "-checkpoint"}
	require.NoError(t, r.Get(t.Context(), key, checkpoint))
	require.Equal(t, []string{"runtime"}, checkpoint.Spec.Containers)
	require.NotContains(t, checkpoint.Labels, api.LabelTemplateName)
	require.Equal(t, "metadata", checkpoint.Labels["editor"])
	require.True(t, ownedByVersion(checkpoint, provisioned))
	require.True(t, *checkpoint.OwnerReferences[0].BlockOwnerDeletion)
	require.Equal(t, "python-agent", provisioned.Labels[api.LabelTemplateName])
	require.Empty(t, provisioned.OwnerReferences)
	require.Equal(t, []string{api.PersistentContentFilesystem}, checkpoint.Spec.PersistentContents)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), key, &api.SandboxTemplate{})))
	require.NotEmpty(t, checkpoint.Annotations[api.AnnotationInitRuntimeRequest])
	checkpoint.Status = api.CheckpointStatus{Phase: api.CheckpointSucceeded, CheckpointId: "backend-snapshot", ObservedGeneration: checkpoint.Generation}
	require.NoError(t, r.Status().Update(t.Context(), checkpoint))
	_, err = r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	ready := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, ready))
	require.Equal(t, api.SandboxTemplatePhaseReady, ready.Status.Phase)
	require.Len(t, ready.Status.Conditions, 1)
	require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(ready.Status.Conditions, buildReadyCondition).Status)
	require.Equal(t, []api.SandboxTemplateArtifact{{Type: "checkpoint", Name: key.Name, Value: "backend-snapshot"}}, ready.Status.Artifacts)
	require.Equal(t, "metadata", ready.Labels["editor"])
	_, err = r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(box), &api.Sandbox{})))
	require.NoError(t, r.Get(t.Context(), key, &api.Checkpoint{}))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), &api.SandboxTemplate{}))
}

func TestSameLogicalNameVersionsExecuteIndependently(t *testing.T) {
	r, version, box, _ := executionFixture(t)
	_, sibling, siblingBox, siblingPod := executionFixture(t)
	sibling.Name, sibling.UID = "st-bcdefghijklmnopqrstu0", "sibling-version-uid"
	sibling.Spec.BuildOptions.Steps[0].Command.Command = "sibling-first"
	sibling.Labels["editor"] = "sibling-metadata"
	siblingBox.Name, siblingBox.UID = sibling.Name, "sibling-sandbox-uid"
	siblingBox.OwnerReferences[0].Name, siblingBox.OwnerReferences[0].UID = sibling.Name, sibling.UID
	siblingPod.Name, siblingPod.UID = sibling.Name, "sibling-pod-uid"
	siblingPod.OwnerReferences[0].Name, siblingPod.OwnerReferences[0].UID = siblingBox.Name, siblingBox.UID
	siblingBox.Status.PodInfo.PodUID = siblingPod.UID
	identity, err := json.Marshal(executionIdentity{SandboxUID: siblingBox.UID, PodUID: siblingPod.UID})
	require.NoError(t, err)
	sibling.Annotations[executionIdentityAnnotation] = string(identity)
	for _, obj := range []client.Object{sibling, siblingBox, siblingPod} {
		obj.SetResourceVersion("")
		require.NoError(t, r.Create(t.Context(), obj))
	}
	require.Equal(t, version.Labels[api.LabelTemplateName], sibling.Labels[api.LabelTemplateName])
	require.Equal(t, version.Namespace, sibling.Namespace)
	require.Empty(t, version.OwnerReferences)
	require.Empty(t, sibling.OwnerReferences)
	started, siblingStarted := make(chan struct{}), make(chan struct{})
	releaseSibling := make(chan struct{})
	var calls, siblingCalls atomic.Int32
	bindBuildRuntime(t, r, box, &buildProcessHandler{start: func(ctx context.Context, request *connect.Request[process.StartRequest], _ *connect.ServerStream[process.StartResponse]) error {
		require.Equal(t, []string{"-l", "-c", "first"}, request.Msg.Process.Args)
		if calls.Add(1) == 1 {
			close(started)
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	bindBuildRuntime(t, r, siblingBox, &buildProcessHandler{start: func(ctx context.Context, request *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		if siblingCalls.Add(1) == 1 {
			require.Equal(t, []string{"-l", "-c", "sibling-first"}, request.Msg.Process.Args)
			close(siblingStarted)
			select {
			case <-releaseSibling:
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			require.Equal(t, []string{"-l", "-c", "second"}, request.Msg.Process.Args)
		}
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true}}}})
	}})
	startExecutionManager(t, r)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)}
	siblingReq := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sibling)}
	for _, request := range []ctrl.Request{req, siblingReq} {
		_, err := r.ReconcileVersion(t.Context(), request)
		require.NoError(t, err)
	}
	for _, signal := range []<-chan struct{}{started, siblingStarted} {
		select {
		case <-signal:
		case <-time.After(3 * time.Second):
			t.Fatal("independent Version execution did not start")
		}
	}
	require.True(t, r.hasWorker(version.UID))
	require.True(t, r.hasWorker(sibling.UID))
	current := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, current))
	require.NoError(t, r.Delete(t.Context(), current))
	_, err = r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	// The deleted Version is gone; its worker is cancelled without a
	// replay, and its Sandbox is left to ownerReference garbage collection.
	require.Eventually(t, func() bool { return !r.hasWorker(version.UID) }, 5*time.Second, 10*time.Millisecond)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), req.NamespacedName, &api.SandboxTemplate{})))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(box), &api.Sandbox{}))
	require.EqualValues(t, 1, calls.Load())
	require.True(t, r.hasWorker(sibling.UID), "deleting a Version must not cancel another Version with the same logical name")
	require.EqualValues(t, 1, siblingCalls.Load())
	currentSibling := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), siblingReq.NamespacedName, currentSibling))
	require.Nil(t, currentSibling.DeletionTimestamp)
	require.Equal(t, api.SandboxTemplatePhaseBuilding, currentSibling.Status.Phase)
	require.Equal(t, buildReasonRunning, meta.FindStatusCondition(currentSibling.Status.Conditions, buildReadyCondition).Reason)
	require.Equal(t, "python-agent", currentSibling.Labels[api.LabelTemplateName])
	require.Empty(t, currentSibling.OwnerReferences)
	currentBox := &api.Sandbox{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(siblingBox), currentBox))
	require.Equal(t, siblingBox.UID, currentBox.UID)
	require.Nil(t, currentBox.DeletionTimestamp)
	currentPod := &corev1.Pod{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(siblingPod), currentPod))
	require.Equal(t, siblingPod.UID, currentPod.UID)
	require.Nil(t, currentPod.DeletionTimestamp)
	close(releaseSibling)
	waitExecutionCondition(t, r, sibling, buildReasonProvisioned)
	require.EqualValues(t, 2, siblingCalls.Load())
	_, err = r.ReconcileVersion(t.Context(), siblingReq)
	require.NoError(t, err)
	checkpoint := &api.Checkpoint{}
	key := client.ObjectKey{Namespace: sibling.Namespace, Name: sibling.Name + "-checkpoint"}
	require.NoError(t, r.Get(t.Context(), key, checkpoint))
	require.True(t, ownedByVersion(checkpoint, sibling))
	require.NotContains(t, checkpoint.Labels, api.LabelTemplateName)
	require.Equal(t, "sibling-metadata", checkpoint.Labels["editor"])
	checkpoint.Status = api.CheckpointStatus{Phase: api.CheckpointSucceeded, CheckpointId: "sibling-backend-snapshot", ObservedGeneration: checkpoint.Generation}
	require.NoError(t, r.Status().Update(t.Context(), checkpoint))
	_, err = r.ReconcileVersion(t.Context(), siblingReq)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), siblingReq.NamespacedName, currentSibling))
	require.Equal(t, api.SandboxTemplatePhaseReady, currentSibling.Status.Phase)
	require.Equal(t, []api.SandboxTemplateArtifact{{Type: "checkpoint", Name: key.Name, Value: "sibling-backend-snapshot"}}, currentSibling.Status.Artifacts)
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 2, siblingCalls.Load())
}

func TestBuildExecutionLostRunningResponse(t *testing.T) {
	r, version, box, _ := executionFixture(t)
	r.Options.Timeout = 30 * time.Minute
	var calls atomic.Int32
	bindBuildRuntime(t, r, box, &buildProcessHandler{start: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		calls.Add(1)
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true}}}})
	}})
	var lost atomic.Bool
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, name string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if err := c.SubResource(name).Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		if current, ok := obj.(*api.SandboxTemplate); ok && name == "status" {
			condition := meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition)
			if condition != nil && condition.Reason == buildReasonRunning && lost.CompareAndSwap(false, true) {
				return apierrors.NewTimeoutError("running result response was lost", 0)
			}
		}
		return nil
	}})
	startExecutionManager(t, r)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)}
	_, err := r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	waitExecutionCondition(t, r, version, buildReasonRunning)
	require.True(t, lost.Load())
	require.Zero(t, calls.Load())
	_, err = r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	require.False(t, r.hasWorker(version.UID))
	_, err = r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, api.SandboxTemplatePhaseFailed, waitExecutionCondition(t, r, version, buildReasonInterrupted).Status.Phase)
	require.Zero(t, calls.Load())
}

func TestBuildExecutionCOPY(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid archive", false: "invalid archive key"}[valid], func(t *testing.T) {
			r, version, box, _ := executionFixture(t)
			hash := strings.Repeat("a", 64)
			if !valid {
				hash = "invalid"
			}
			version.Spec.BuildOptions.Steps = []api.TemplateBuildStep{{Copy: &api.TemplateBuildCopy{Src: "app.py", Dest: "/app/app.py", FilesHash: hash, Mode: ptr.To(int32(0))}}}
			current := &api.SandboxTemplate{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
			current.Spec = version.Spec
			require.NoError(t, r.Update(t.Context(), current))
			var calls atomic.Int32
			bindBuildRuntime(t, r, box, &buildProcessHandler{start: func(_ context.Context, request *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				calls.Add(1)
				require.Contains(t, request.Msg.Process.Args[2], "/blobs/"+hash+".tar.gz")
				require.Contains(t, request.Msg.Process.Args[2], "permissions='0000'")
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true}}}})
			}})
			startExecutionManager(t, r)
			_, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)})
			require.NoError(t, err)
			if valid {
				waitExecutionCondition(t, r, version, buildReasonProvisioned)
				require.EqualValues(t, 1, calls.Load())
			} else {
				waitExecutionCondition(t, r, version, buildReasonExecutionFailed)
				require.Zero(t, calls.Load())
			}
		})
	}
}

func TestBuildExecutionInterruptions(t *testing.T) {
	for _, mode := range []string{"leadership lost", "logical label removed", "Version deleting", "missing end", "nonzero exit", "nonzero with process error", "nonzero then stream error", "transport unknown", "transport invalid argument", "budget expired"} {
		t.Run(mode, func(t *testing.T) {
			r, version, box, _ := executionFixture(t)
			if mode == "budget expired" {
				version.Status.Conditions = []metav1.Condition{{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonRunning, LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Minute))}}
				require.NoError(t, r.Status().Update(t.Context(), version))
			}
			started := make(chan struct{})
			var calls atomic.Int32
			bindBuildRuntime(t, r, box, &buildProcessHandler{start: func(ctx context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				calls.Add(1)
				close(started)
				if mode == "missing end" {
					return nil
				}
				if strings.HasPrefix(mode, "nonzero") {
					end := &process.ProcessEvent_EndEvent{Exited: true, ExitCode: 23}
					if mode == "nonzero with process error" {
						end.Error = ptr.To("exit status 23")
					}
					if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: end}}}); err != nil {
						return err
					}
					if mode == "nonzero then stream error" {
						return connect.NewError(connect.CodeUnavailable, errors.New("stream ended after command exit"))
					}
					return nil
				}
				if mode == "transport unknown" {
					return connect.NewError(connect.CodeUnknown, errors.New("unknown stream outcome"))
				}
				if mode == "transport invalid argument" {
					return connect.NewError(connect.CodeInvalidArgument, errors.New("rejected build command"))
				}
				if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 123}}}}); err != nil {
					return err
				}
				<-ctx.Done()
				return ctx.Err()
			}})
			cancel := startExecutionManager(t, r)
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)}
			_, err := r.ReconcileVersion(t.Context(), req)
			require.NoError(t, err)
			if mode == "leadership lost" || mode == "logical label removed" || mode == "Version deleting" {
				select {
				case <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("build command did not start")
				}
				if mode == "leadership lost" {
					cancel()
				} else if mode == "Version deleting" {
					current := &api.SandboxTemplate{}
					require.NoError(t, r.Get(t.Context(), req.NamespacedName, current))
					require.NoError(t, r.Delete(t.Context(), current))
					_, err = r.ReconcileVersion(t.Context(), req)
					require.NoError(t, err)
					// The deleted Version is gone, so no failure condition can
					// be recorded on it: the worker is cancelled without a
					// replay.
					require.Eventually(t, func() bool { return !r.hasWorker(version.UID) }, 5*time.Second, 10*time.Millisecond)
					require.True(t, apierrors.IsNotFound(r.Get(t.Context(), req.NamespacedName, &api.SandboxTemplate{})))
					require.EqualValues(t, 1, calls.Load())
					return
				} else {
					current := &api.SandboxTemplate{}
					require.NoError(t, r.Get(t.Context(), req.NamespacedName, current))
					delete(current.Labels, api.LabelTemplateName)
					require.NoError(t, r.Update(t.Context(), current))
					_, err = r.ReconcileVersion(t.Context(), req)
					require.NoError(t, err)
				}
			}
			reason := buildReasonInterrupted
			if strings.HasPrefix(mode, "nonzero") {
				reason = buildReasonExecutionFailed
			} else if mode == "budget expired" {
				reason = buildReasonTimeout
			} else if mode == "transport invalid argument" {
				reason = buildReasonExecutionFailed
			}
			current := waitExecutionCondition(t, r, version, reason)
			require.Equal(t, api.SandboxTemplatePhaseFailed, current.Status.Phase)
			if reason == buildReasonExecutionFailed {
				require.Equal(t, "Build step 1 failed.", meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition).Message)
			}
			before := calls.Load()
			_, err = r.ReconcileVersion(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, before, calls.Load())
			if mode == "budget expired" {
				require.Zero(t, calls.Load())
			}
		})
	}
}

func TestExecutionStatusPreservesKnownResult(t *testing.T) {
	r, version, _, _ := executionFixture(t)
	version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseBuilding, ObservedGeneration: version.Generation, Conditions: []metav1.Condition{{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonRunning, ObservedGeneration: version.Generation, LastTransitionTime: metav1.Now()}}}
	require.NoError(t, r.Status().Update(t.Context(), version))
	stale := version.DeepCopy()
	version.Status.Conditions[0].Reason = buildReasonProvisioned
	require.NoError(t, r.Status().Update(t.Context(), version))
	_, err := r.reconcileExecution(t.Context(), stale)
	require.True(t, apierrors.IsConflict(err), "%v", err)
	// The live build turns terminal while the stale view is still in flight.
	current := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
	require.Equal(t, api.SandboxTemplatePhaseBuilding, current.Status.Phase)
	require.Equal(t, buildReasonProvisioned, meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition).Reason)
	terminal := api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseFailed, ObservedGeneration: current.Generation, Conditions: []metav1.Condition{{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonInterrupted, ObservedGeneration: current.Generation, LastTransitionTime: metav1.NewTime(time.Now().Truncate(time.Second))}}}
	current.Status = terminal
	require.NoError(t, r.Status().Update(t.Context(), current))
	require.NoError(t, r.executionStatus(t.Context(), stale, api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, buildReasonInterrupted, "late failure"))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
	require.Equal(t, terminal, current.Status)
}

func TestExecutionStatusRetriesMetadataConflict(t *testing.T) {
	for _, tt := range []struct {
		name      string
		conflicts int32
		cancel    bool
		wantError error
		attempts  int32
	}{
		{name: "metadata edits converge", conflicts: 2, attempts: 3},
		{name: "conflicts exhaust default backoff", conflicts: int32(retry.DefaultBackoff.Steps), wantError: wait.ErrWaitTimeout, attempts: int32(retry.DefaultBackoff.Steps)},
		{name: "cancellation stops conflict retries", conflicts: 1, cancel: true, wantError: context.Canceled, attempts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, _, _ := executionFixture(t)
			base := r.Client.(client.WithWatch)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var attempts atomic.Int32
			r.Client = interceptor.NewClient(base, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if current, ok := obj.(*api.SandboxTemplate); ok && attempts.Add(1) <= tt.conflicts {
					edited := current.DeepCopy()
					edited.Labels["editor"] = "winner"
					if err := c.Update(ctx, edited); err != nil {
						return err
					}
					if tt.cancel {
						cancel()
					}
				}
				return nil
			}})
			err := r.executionStatus(ctx, version, api.SandboxTemplatePhaseBuilding, metav1.ConditionFalse, buildReasonProvisioned, "known result")
			if tt.wantError != nil {
				require.ErrorIs(t, err, tt.wantError)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.attempts, attempts.Load())
			current := &api.SandboxTemplate{}
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(version), current))
			require.Equal(t, "winner", current.Labels["editor"])
			condition := meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition)
			if tt.wantError != nil {
				require.Nil(t, condition)
			} else {
				require.NotNil(t, condition)
				require.Equal(t, buildReasonProvisioned, condition.Reason)
			}
		})
	}
}

func TestBuildExecutionRestartDoesNotReplay(t *testing.T) {
	r, version, box, _ := executionFixture(t)
	r.Options.Timeout = 30 * time.Minute
	started := time.Now().Add(-4 * time.Minute)
	version.CreationTimestamp = metav1.NewTime(started.Add(-time.Minute))
	require.NoError(t, r.Update(t.Context(), version))
	version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseBuilding, ObservedGeneration: version.Generation, Conditions: []metav1.Condition{
		{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonRunning, LastTransitionTime: metav1.NewTime(started)},
	}}
	require.NoError(t, r.Status().Update(t.Context(), version))
	var calls atomic.Int32
	bindBuildRuntime(t, r, box, &buildProcessHandler{start: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		calls.Add(1)
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true}}}})
	}})
	startExecutionManager(t, r)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)}
	_, err := r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, api.SandboxTemplatePhaseFailed, waitExecutionCondition(t, r, version, buildReasonInterrupted).Status.Phase)
	require.Zero(t, calls.Load())
	_, err = r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(box), &api.Sandbox{})))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), &api.SandboxTemplate{}))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), &api.SandboxTemplate{}))
}

func TestBuildCaptureBoundaries(t *testing.T) {
	for _, mode := range []string{"pending", "worker exiting", "missing backend ID", "failed capture", "changed scope", "changed init metadata", "container restarted", "budget expired"} {
		t.Run(mode, func(t *testing.T) {
			r, version, box, pod := executionFixture(t)
			version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseBuilding, ObservedGeneration: version.Generation, Conditions: []metav1.Condition{
				{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonProvisioned, ObservedGeneration: version.Generation, LastTransitionTime: metav1.Now()},
			}}
			require.NoError(t, r.Status().Update(t.Context(), version))
			identity, err := parseExecutionIdentity(version.Annotations[executionIdentityAnnotation])
			require.NoError(t, err)
			identity.ContainerID = pod.Status.ContainerStatuses[0].ContainerID
			data, err := json.Marshal(identity)
			require.NoError(t, err)
			version.Annotations[executionIdentityAnnotation] = string(data)
			require.NoError(t, r.Update(t.Context(), version))
			currentBox := &api.Sandbox{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(box), currentBox))
			currentBox.Annotations = map[string]string{api.AnnotationInitRuntimeRequest: `{"accessToken":"test-runtime-token"}`}
			require.NoError(t, r.Update(t.Context(), currentBox))
			if mode == "worker exiting" {
				_, workerCancel := context.WithCancel(t.Context())
				t.Cleanup(workerCancel)
				r.workers = map[types.UID]*buildWorker{version.UID: {key: client.ObjectKeyFromObject(version), cancel: workerCancel}}
			}
			_, err = r.reconcileExecution(t.Context(), version)
			require.NoError(t, err)
			key := client.ObjectKey{Namespace: version.Namespace, Name: version.Name + "-checkpoint"}
			checkpoint := &api.Checkpoint{}
			require.NoError(t, r.Get(t.Context(), key, checkpoint))
			checkpoint.Status.ObservedGeneration = checkpoint.Generation
			if mode != "pending" {
				checkpoint.Status.Phase = api.CheckpointSucceeded
				checkpoint.Status.CheckpointId = "backend-id"
			}
			if mode == "missing backend ID" {
				checkpoint.Status.CheckpointId = ""
			} else if mode == "failed capture" {
				checkpoint.Status.Phase = api.CheckpointFailed
			}
			require.NoError(t, r.Status().Update(t.Context(), checkpoint))
			switch mode {
			case "changed scope":
				checkpoint.Spec.PersistentContents = []string{api.PersistentContentMemory}
				require.NoError(t, r.Update(t.Context(), checkpoint))
			case "changed init metadata":
				checkpoint.Annotations[api.AnnotationInitRuntimeRequest] = "changed"
				require.NoError(t, r.Update(t.Context(), checkpoint))
			case "container restarted":
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
				require.NoError(t, r.Status().Update(t.Context(), pod))
			case "budget expired":
				started := metav1.NewTime(time.Now().Add(-2 * time.Minute).Truncate(time.Second))
				condition := meta.FindStatusCondition(version.Status.Conditions, buildReadyCondition)
				condition.Reason, condition.LastTransitionTime = buildReasonRunning, started
				require.NoError(t, r.Status().Update(t.Context(), version))
				require.NoError(t, r.executionStatus(t.Context(), version, api.SandboxTemplatePhaseBuilding, metav1.ConditionFalse, buildReasonProvisioned, "known result"))
				require.True(t, started.Time.Equal(meta.FindStatusCondition(version.Status.Conditions, buildReadyCondition).LastTransitionTime.Time))
			}
			_, err = r.reconcileCapture(t.Context(), version)
			require.NoError(t, err)
			current := &api.SandboxTemplate{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
			if mode == "pending" {
				require.NotEqual(t, api.SandboxTemplatePhaseReady, current.Status.Phase)
			} else if mode == "worker exiting" {
				require.Equal(t, api.SandboxTemplatePhaseReady, current.Status.Phase)
				require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition).Status)
			} else {
				require.Equal(t, api.SandboxTemplatePhaseFailed, current.Status.Phase)
				require.Empty(t, current.Status.Artifacts)
			}
		})
	}
}

func TestBuildImageConvergence(t *testing.T) {
	for _, mode := range []string{"old container", "changed image", "terminated container", "terminated original container", "corrupt update state", "update in progress"} {
		t.Run(mode, func(t *testing.T) {
			r, version, box, pod := executionFixture(t)
			status := &pod.Status.ContainerStatuses[0]
			oldImageID := status.ImageID
			status.ContainerID = "containerd://replacement"
			switch mode {
			case "old container":
				status.ContainerID = "containerd://original"
			case "changed image":
				oldImageID = "sha256:" + strings.Repeat("b", 64)
			case "terminated container", "terminated original container":
				status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
				if mode == "terminated original container" {
					status.ContainerID = "containerd://original"
				}
			}
			last := inplaceupdate.InPlaceUpdateContainerStatus{ImageID: oldImageID}
			state, err := json.Marshal(inplaceupdate.InPlaceUpdateState{UpdateImages: true, LastContainerStatuses: map[string]inplaceupdate.InPlaceUpdateContainerStatus{"runtime": last}})
			require.NoError(t, err)
			annotation := string(state)
			if mode == "corrupt update state" {
				annotation = "{"
			}
			pod.Annotations = map[string]string{inplaceupdate.PodAnnotationInPlaceUpdateStateKey: annotation}
			desiredStatus := pod.Status.DeepCopy()
			require.NoError(t, r.Update(t.Context(), pod))
			pod.Status = *desiredStatus
			require.NoError(t, r.Status().Update(t.Context(), pod))
			session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace, sandboxUID: box.UID, podUID: pod.UID, initialContainerID: "containerd://original", imageChanged: true}
			_, ready, err := r.executionSnapshot(t.Context(), session, version.Spec.Template.Spec.Containers[0].Image, box.Status.UpdateRevision)
			switch mode {
			case "terminated container", "corrupt update state":
				require.ErrorIs(t, err, errExecutionInterrupted)
			default:
				require.NoError(t, err)
				require.Equal(t, mode == "changed image", ready)
			}
		})
	}
}

func TestExecutionStatusRunningGuards(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*api.SandboxTemplate)
	}{
		{name: "terminal Version cannot restart a build", mutate: func(v *api.SandboxTemplate) {
			v.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseReady, ObservedGeneration: v.Generation}
		}},
		{name: "existing condition blocks a second Running write", mutate: func(v *api.SandboxTemplate) {
			v.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseBuilding, Conditions: []metav1.Condition{{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonProvisioned}}}
		}},
		{name: "deleting Version blocks a Running write", mutate: func(v *api.SandboxTemplate) {
			v.DeletionTimestamp = ptr.To(metav1.Now())
			v.Finalizers = []string{"test/retain"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			version := modelFixtures()
			tt.mutate(version)
			r := modelReconciler(t, version)
			err := r.executionStatus(t.Context(), version, api.SandboxTemplatePhaseBuilding, metav1.ConditionFalse, buildReasonRunning, "start")
			require.ErrorIs(t, err, errExecutionInterrupted)
		})
	}
}

func TestBuildBudgetHelpers(t *testing.T) {
	r, version, _, _ := executionFixture(t)
	started := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	running := metav1.Condition{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonRunning, LastTransitionTime: started}
	require.Equal(t, started.Add(r.Options.Timeout), r.buildDeadline(&running))
	defaults := &Reconciler{Options: Options{}}
	require.Equal(t, started.Add(DefaultBuildTimeout), defaults.buildDeadline(&running))
	version.Status.Conditions = []metav1.Condition{running}
	require.Equal(t, retryInterval, r.buildRequeueAfter(version))
	require.NoError(t, r.failRunningBuild(t.Context(), version, buildReasonTimeout, "expired"))
	version.Status.Conditions[0].Reason = buildReasonProvisioned
	require.NoError(t, r.failRunningBuild(t.Context(), version, buildReasonTimeout, "expired"))
	current := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
	require.Equal(t, api.SandboxTemplatePhaseFailed, current.Status.Phase)
}

func TestRunExecutionBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name    string
		runtime bool
		mutate  func(*testing.T, *Reconciler, *api.SandboxTemplate, *api.Sandbox, *corev1.Pod)
		phase   api.SandboxTemplateBuildPhase
		reason  string
	}{
		{
			name: "a previously started build is not replayed",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, _ *api.Sandbox, _ *corev1.Pod) {
				version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseBuilding, ObservedGeneration: version.Generation, Conditions: []metav1.Condition{{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonRunning, LastTransitionTime: metav1.Now()}}}
				require.NoError(t, r.Status().Update(t.Context(), version))
			},
			phase:  api.SandboxTemplatePhaseFailed,
			reason: buildReasonInterrupted,
		},
		{
			name: "budget expires before the Sandbox binds",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, box *api.Sandbox, pod *corev1.Pod) {
				r.Options.Timeout = 150 * time.Millisecond
				current := &api.SandboxTemplate{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
				current.Annotations[executionIdentityAnnotation] = ""
				require.NoError(t, r.Update(t.Context(), current))
				require.NoError(t, r.Delete(t.Context(), box))
				require.NoError(t, r.Delete(t.Context(), pod))
			},
			phase:  api.SandboxTemplatePhaseFailed,
			reason: buildReasonTimeout,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, box, pod := executionFixture(t)
			handler := &buildProcessHandler{start: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true}}}})
			}}
			if tt.runtime {
				bindBuildRuntime(t, r, box, handler)
			}
			if tt.mutate != nil {
				tt.mutate(t, r, version, box, pod)
			}
			err := r.runExecution(t.Context(), version)
			require.NoError(t, err)
			current := &api.SandboxTemplate{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
			require.Equal(t, tt.phase, current.Status.Phase)
			require.Equal(t, tt.reason, meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition).Reason)
		})
	}
}

func TestBindExecutionSandboxBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mutate    func(*testing.T, *Reconciler, *api.SandboxTemplate, *api.Sandbox)
		funcs     interceptor.Funcs
		wantError error
		deadline  bool
	}{
		{
			name: "missing Version interrupts",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, _ *api.Sandbox) {
				removeStoredVersion(t, r, version)
			},
			wantError: errExecutionInterrupted,
		},
		{
			name: "corrupt identity annotation fails",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, _ *api.Sandbox) {
				setStoredIdentity(t, r, version, "{")
			},
		},
		{
			name: "Sandbox not yet created keeps waiting",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, box *api.Sandbox) {
				setStoredIdentity(t, r, version, "")
				require.NoError(t, r.Delete(t.Context(), box))
			},
			deadline: true,
		},
		{
			name: "bound Pod identity is lost",
			mutate: func(t *testing.T, r *Reconciler, _ *api.SandboxTemplate, box *api.Sandbox) {
				require.NoError(t, r.Delete(t.Context(), box))
			},
			wantError: errExecutionInterrupted,
		},
		{
			name:      "Sandbox read failure",
			funcs:     failGetFor(&api.Sandbox{}, errors.New("sandbox read unavailable")),
			wantError: errors.New("sandbox read unavailable"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, box, _ := executionFixture(t)
			if tt.mutate != nil {
				tt.mutate(t, r, version, box)
			}
			if tt.funcs.Get != nil {
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), tt.funcs)
			}
			ctx := t.Context()
			if tt.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
			}
			session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace}
			err := r.bindExecutionSandbox(ctx, session)
			if tt.deadline {
				require.ErrorIs(t, err, context.DeadlineExceeded)
				return
			}
			if tt.wantError != nil {
				require.ErrorContains(t, err, tt.wantError.Error())
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestReadExecutionSandboxIdentityDivergence(t *testing.T) {
	for _, tt := range []struct {
		name      string
		identity  string
		interrupt bool
	}{
		{name: "corrupt identity annotation fails", identity: "{"},
		{name: "identity recorded for another execution", identity: `{"sandboxUID":"replacement"}`, interrupt: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, box, _ := executionFixture(t)
			setStoredIdentity(t, r, version, tt.identity)
			session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace, sandboxUID: box.UID, podUID: "pod-uid"}
			_, err := r.readExecutionSandbox(t.Context(), session)
			if tt.interrupt {
				require.ErrorIs(t, err, errExecutionInterrupted)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestExecutionSnapshotPodAccess(t *testing.T) {
	t.Run("Pod read failure", func(t *testing.T) {
		r, version, box, _ := executionFixture(t)
		failure := errors.New("pod read unavailable")
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), failGetFor(&corev1.Pod{}, failure))
		session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace, sandboxUID: box.UID, podUID: "pod-uid"}
		_, _, err := r.executionSnapshot(t.Context(), session, version.Spec.Template.Spec.Containers[0].Image, box.Status.UpdateRevision)
		require.ErrorIs(t, err, failure)
	})
	t.Run("unrelated container statuses are skipped", func(t *testing.T) {
		r, version, box, pod := executionFixture(t)
		sidecar := corev1.ContainerStatus{Name: "sidecar", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, ContainerID: "containerd://sidecar"}
		pod.Status.ContainerStatuses = append([]corev1.ContainerStatus{sidecar}, pod.Status.ContainerStatuses...)
		require.NoError(t, r.Status().Update(t.Context(), pod))
		session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace, sandboxUID: box.UID, podUID: "pod-uid"}
		_, ready, err := r.executionSnapshot(t.Context(), session, version.Spec.Template.Spec.Containers[0].Image, box.Status.UpdateRevision)
		require.NoError(t, err)
		require.True(t, ready)
	})
}

func TestRecordExecutionIdentityBoundaries(t *testing.T) {
	t.Run("corrupt identity annotation fails", func(t *testing.T) {
		r, version, box, pod := executionFixture(t)
		setStoredIdentity(t, r, version, "{")
		session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace, sandboxUID: box.UID, podUID: pod.UID}
		require.Error(t, r.recordExecutionIdentity(t.Context(), session))
	})
	t.Run("recording initializes the annotation map", func(t *testing.T) {
		r, version, box, pod := executionFixture(t)
		mutateStoredVersion(t, r, version, func(v *api.SandboxTemplate) { v.Annotations = nil })
		session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace, sandboxUID: box.UID, podUID: pod.UID, containerID: "containerd://original"}
		require.NoError(t, r.recordExecutionIdentity(t.Context(), session))
		updated := &api.SandboxTemplate{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), updated))
		recorded, err := parseExecutionIdentity(updated.Annotations[executionIdentityAnnotation])
		require.NoError(t, err)
		require.Equal(t, executionIdentity{SandboxUID: box.UID, PodUID: pod.UID, ContainerID: "containerd://original"}, recorded)
	})
}

func TestBindBuildRuntimeBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mutate    func(*testing.T, *Reconciler, *api.SandboxTemplate, *api.Sandbox)
		mode      string
		wantToken string
		wantError error
		notFound  bool
	}{
		{
			name: "Sandbox lost during binding",
			mutate: func(t *testing.T, r *Reconciler, _ *api.SandboxTemplate, box *api.Sandbox) {
				require.NoError(t, r.Delete(t.Context(), box))
			},
			notFound: true,
		},
		{
			name: "runtime container missing",
			mutate: func(t *testing.T, r *Reconciler, _ *api.SandboxTemplate, box *api.Sandbox) {
				mutateStoredBox(t, r, box, func(b *api.Sandbox) { b.Spec.Template = nil })
			},
			wantError: errExecutionInterrupted,
		},
		{
			name: "existing token is reused",
			mutate: func(t *testing.T, r *Reconciler, _ *api.SandboxTemplate, box *api.Sandbox) {
				mutateStoredBox(t, r, box, func(b *api.Sandbox) {
					b.Annotations = map[string]string{api.AnnotationRuntimeAccessToken: "existing-runtime-token"}
				})
			},
			wantToken: "existing-runtime-token",
		},
		{name: "metadata conflict retries", mode: "conflict"},
		{name: "patch failure", mode: "patch failure"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, box, pod := executionFixture(t)
			if tt.mutate != nil {
				tt.mutate(t, r, version, box)
			}
			failure := errors.New("runtime patch unavailable")
			var attempts atomic.Int32
			if tt.mode == "conflict" || tt.mode == "patch failure" {
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*api.Sandbox); ok {
						if tt.mode == "patch failure" {
							return failure
						}
						if attempts.Add(1) == 1 {
							return apierrors.NewConflict(schema.GroupResource{Group: api.GroupVersion.Group, Resource: "sandboxes"}, obj.GetName(), errors.New("edited"))
						}
					}
					return c.Patch(ctx, obj, patch, opts...)
				}})
			}
			session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace, sandboxUID: box.UID, podUID: pod.UID}
			bound, token, err := r.bindBuildRuntime(t.Context(), session, version, "fresh-token")
			switch {
			case tt.wantError != nil:
				require.ErrorIs(t, err, tt.wantError)
				require.Nil(t, bound)
			case tt.notFound:
				require.True(t, apierrors.IsNotFound(err), "%v", err)
				require.Nil(t, bound)
			case tt.mode == "patch failure":
				require.ErrorIs(t, err, failure)
				require.Nil(t, bound)
			default:
				require.NoError(t, err)
				require.NotNil(t, bound)
				if tt.wantToken != "" {
					require.Equal(t, tt.wantToken, token)
					require.Equal(t, tt.wantToken, bound.Annotations[api.AnnotationRuntimeAccessToken])
				}
				if tt.mode == "conflict" {
					require.EqualValues(t, 2, attempts.Load())
				}
			}
		})
	}
}

func TestExecuteBoundaries(t *testing.T) {
	interrupted := func(t *testing.T, err error) {
		t.Helper()
		require.ErrorIs(t, err, errExecutionInterrupted)
	}
	for _, tt := range []struct {
		name     string
		runtime  bool
		initFail bool
		mutate   func(*testing.T, *Reconciler, *api.SandboxTemplate, *api.Sandbox, *corev1.Pod)
		funcs    func() interceptor.Funcs
		assert   func(*testing.T, error)
	}{
		{
			name: "Sandbox lost before execution",
			mutate: func(t *testing.T, r *Reconciler, _ *api.SandboxTemplate, box *api.Sandbox, _ *corev1.Pod) {
				require.NoError(t, r.Delete(t.Context(), box))
			},
			assert: func(t *testing.T, err error) {
				t.Helper()
				require.True(t, apierrors.IsNotFound(err), "%v", err)
			},
		},
		{
			name: "runtime container missing after binding",
			mutate: func(t *testing.T, r *Reconciler, _ *api.SandboxTemplate, box *api.Sandbox, _ *corev1.Pod) {
				mutateStoredBox(t, r, box, func(b *api.Sandbox) { b.Spec.Template = nil })
			},
			assert: interrupted,
		},
		{
			name: "Pod replaced after binding",
			funcs: func() interceptor.Funcs {
				var reads atomic.Int32
				return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Pod); ok && reads.Add(1) >= 2 {
						return apierrors.NewNotFound(corev1.SchemeGroupVersion.WithResource("pods").GroupResource(), key.Name)
					}
					return c.Get(ctx, key, obj, opts...)
				}}
			},
			assert: interrupted,
		},
		{
			name:   "runtime transport missing",
			assert: func(t *testing.T, err error) { t.Helper(); require.Error(t, err) },
		},
		{
			name: "runtime URL invalid",
			mutate: func(t *testing.T, r *Reconciler, _ *api.SandboxTemplate, box *api.Sandbox, _ *corev1.Pod) {
				mutateStoredBox(t, r, box, func(b *api.Sandbox) { b.Annotations = map[string]string{api.AnnotationRuntimeURL: "://not-a-url"} })
			},
			assert: func(t *testing.T, err error) { t.Helper(); require.Error(t, err) },
		},
		{
			name:     "runtime init fails",
			initFail: true,
			assert:   func(t *testing.T, err error) { t.Helper(); require.Error(t, err) },
		},
		{
			name:    "empty build step",
			runtime: true,
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, _ *api.Sandbox, _ *corev1.Pod) {
				version.Spec.BuildOptions.Steps = []api.TemplateBuildStep{{}}
				current := &api.SandboxTemplate{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
				current.Spec = version.Spec
				require.NoError(t, r.Update(t.Context(), current))
			},
			assert: func(t *testing.T, err error) {
				t.Helper()
				var stepError *buildStepError
				require.ErrorAs(t, err, &stepError)
				require.Equal(t, "build step 1 failed", err.Error())
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, box, pod := executionFixture(t)
			handler := &buildProcessHandler{start: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true}}}})
			}}
			if tt.runtime {
				bindBuildRuntime(t, r, box, handler)
			}
			if tt.initFail {
				mux := http.NewServeMux()
				path, processHandler := processconnect.NewProcessHandler(handler)
				mux.Handle(path, processHandler)
				mux.HandleFunc("/init", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
				server := httptest.NewServer(mux)
				t.Cleanup(server.Close)
				current := &api.Sandbox{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(box), current))
				current.Annotations = map[string]string{api.AnnotationRuntimeURL: server.URL}
				require.NoError(t, r.Update(t.Context(), current))
			}
			if tt.mutate != nil {
				tt.mutate(t, r, version, box, pod)
			}
			if tt.funcs != nil {
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), tt.funcs())
			}
			session := &executionSession{versionName: version.Name, versionUID: version.UID, namespace: version.Namespace, sandboxUID: box.UID, podUID: pod.UID}
			tt.assert(t, r.execute(t.Context(), session, version))
		})
	}
}

func TestBuildCaptureFailurePaths(t *testing.T) {
	failed := func(reason string) func(*testing.T, ctrl.Result, error, *api.SandboxTemplate) {
		return func(t *testing.T, _ ctrl.Result, err error, current *api.SandboxTemplate) {
			t.Helper()
			require.NoError(t, err)
			require.Equal(t, api.SandboxTemplatePhaseFailed, current.Status.Phase)
			require.Equal(t, reason, meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition).Reason)
		}
	}
	errContains := func(text string) func(*testing.T, ctrl.Result, error, *api.SandboxTemplate) {
		return func(t *testing.T, _ ctrl.Result, err error, _ *api.SandboxTemplate) {
			t.Helper()
			require.ErrorContains(t, err, text)
		}
	}
	phaseStays := func(phase api.SandboxTemplateBuildPhase) func(*testing.T, ctrl.Result, error, *api.SandboxTemplate) {
		return func(t *testing.T, _ ctrl.Result, err error, current *api.SandboxTemplate) {
			t.Helper()
			require.NoError(t, err)
			require.Equal(t, phase, current.Status.Phase)
		}
	}
	// prepare runs a first reconcileCapture that creates the checkpoint and
	// leaves it in a state that the second call must observe.
	prepare := func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, persist func(*testing.T, *Reconciler, *api.Checkpoint)) {
		t.Helper()
		_, err := r.reconcileCapture(t.Context(), version)
		require.NoError(t, err)
		checkpoint := &api.Checkpoint{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: version.Namespace, Name: version.Name + "-checkpoint"}, checkpoint))
		persist(t, r, checkpoint)
	}
	for _, tt := range []struct {
		name    string
		mutate  func(*testing.T, *Reconciler, *api.SandboxTemplate, *api.Sandbox, *corev1.Pod)
		funcs   interceptor.Funcs
		prepare func(*testing.T, *Reconciler, *api.Checkpoint)
		assert  func(*testing.T, ctrl.Result, error, *api.SandboxTemplate)
	}{
		{
			name: "corrupt identity annotation",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, _ *api.Sandbox, _ *corev1.Pod) {
				version.Annotations[executionIdentityAnnotation] = "{"
				setStoredIdentity(t, r, version, "{")
			},
			assert: errContains("decode template execution identity"),
		},
		{
			name: "missing business runtime definition",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, _ *api.Sandbox, _ *corev1.Pod) {
				version.Spec.Template = nil
				mutateStoredVersion(t, r, version, func(v *api.SandboxTemplate) { v.Spec.Template = nil })
			},
			assert: failed(buildReasonCaptureConflict),
		},
		{
			name: "provisioned Sandbox lost",
			mutate: func(t *testing.T, r *Reconciler, _ *api.SandboxTemplate, box *api.Sandbox, _ *corev1.Pod) {
				require.NoError(t, r.Delete(t.Context(), box))
			},
			assert: failed(buildReasonInterrupted),
		},
		{
			name: "provisioned runtime template missing",
			mutate: func(t *testing.T, r *Reconciler, _ *api.SandboxTemplate, box *api.Sandbox, _ *corev1.Pod) {
				mutateStoredBox(t, r, box, func(b *api.Sandbox) { b.Spec.Template = nil })
			},
			assert: failed(buildReasonCaptureConflict),
		},
		{
			name: "business container not ready",
			mutate: func(t *testing.T, r *Reconciler, _ *api.SandboxTemplate, _ *api.Sandbox, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].Ready = false
				require.NoError(t, r.Status().Update(t.Context(), pod))
			},
			assert: phaseStays(api.SandboxTemplatePhaseBuilding),
		},
		{
			name: "capture without checkpoint",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, _ *api.Sandbox, _ *corev1.Pod) {
				version.Spec.Checkpoint.Enable = false
				mutateStoredVersion(t, r, version, func(v *api.SandboxTemplate) { v.Spec.Checkpoint.Enable = false })
			},
			assert: func(t *testing.T, _ ctrl.Result, err error, current *api.SandboxTemplate) {
				t.Helper()
				require.NoError(t, err)
				require.Equal(t, api.SandboxTemplatePhaseReady, current.Status.Phase)
				require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition).Status)
			},
		},
		{
			name: "checkpoint progress pending",
			prepare: func(t *testing.T, r *Reconciler, checkpoint *api.Checkpoint) {
				checkpoint.Generation = 1
				require.NoError(t, r.Update(t.Context(), checkpoint))
			},
			assert: phaseStays(api.SandboxTemplatePhaseBuilding),
		},
		{
			name:   "snapshot read failure",
			funcs:  failGetFor(&corev1.Pod{}, errors.New("pod read unavailable")),
			assert: errContains("pod read unavailable"),
		},
		{
			name:   "checkpoint read failure",
			funcs:  failGetFor(&api.Checkpoint{}, errors.New("checkpoint read unavailable")),
			assert: errContains("checkpoint read unavailable"),
		},
		{
			name: "checkpoint create race",
			funcs: interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*api.Checkpoint); ok {
						return apierrors.NewNotFound(api.GroupVersion.WithResource("checkpoints").GroupResource(), key.Name)
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*api.Checkpoint); ok {
						return apierrors.NewAlreadyExists(api.GroupVersion.WithResource("checkpoints").GroupResource(), obj.GetName())
					}
					return c.Create(ctx, obj, opts...)
				},
			},
			assert: phaseStays(api.SandboxTemplatePhaseBuilding),
		},
		{
			name: "status publication fails",
			funcs: interceptor.Funcs{SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				return errors.New("status write unavailable")
			}},
			prepare: func(t *testing.T, r *Reconciler, checkpoint *api.Checkpoint) {
				checkpoint.Status = api.CheckpointStatus{Phase: api.CheckpointSucceeded, CheckpointId: "backend-id", ObservedGeneration: checkpoint.Generation}
				require.NoError(t, r.Status().Update(t.Context(), checkpoint))
			},
			assert: errContains("status write unavailable"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, box, pod := executionFixture(t)
			version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseBuilding, ObservedGeneration: version.Generation, Conditions: []metav1.Condition{
				{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonProvisioned, ObservedGeneration: version.Generation, LastTransitionTime: metav1.Now()},
			}}
			require.NoError(t, r.Status().Update(t.Context(), version))
			identity, err := parseExecutionIdentity(version.Annotations[executionIdentityAnnotation])
			require.NoError(t, err)
			identity.ContainerID = pod.Status.ContainerStatuses[0].ContainerID
			data, err := json.Marshal(identity)
			require.NoError(t, err)
			version.Annotations[executionIdentityAnnotation] = string(data)
			require.NoError(t, r.Update(t.Context(), version))
			currentBox := &api.Sandbox{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(box), currentBox))
			currentBox.Annotations = map[string]string{api.AnnotationInitRuntimeRequest: `{"accessToken":"test-runtime-token"}`}
			require.NoError(t, r.Update(t.Context(), currentBox))
			if tt.mutate != nil {
				tt.mutate(t, r, version, box, pod)
			}
			if tt.funcs.Get != nil || tt.funcs.Create != nil || tt.funcs.SubResourcePatch != nil {
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), tt.funcs)
			}
			if tt.prepare != nil {
				prepare(t, r, version, tt.prepare)
			}
			result, err := r.reconcileCapture(t.Context(), version)
			current := &api.SandboxTemplate{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
			tt.assert(t, result, err, current)
		})
	}
}
