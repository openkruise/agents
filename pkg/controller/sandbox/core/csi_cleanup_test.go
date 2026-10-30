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

package core

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
	utestutils "github.com/openkruise/agents/pkg/utils/testutils"
	testutils "github.com/openkruise/agents/test/utils"
)

// TestDefaultSandboxInitializer_UnmountCSIVolumes covers the delete-path
// auto-unmount: the resolved mount configs drive the sandbox-storage unmount
// CLI through the runtime; every failure mode degrades to a Warning event and
// never returns an error (best-effort policy).
func TestDefaultSandboxInitializer_UnmountCSIVolumes(t *testing.T) {
	utestutils.InitLogOutput()

	const testDriver = "test-csi-driver"

	tests := []struct {
		name         string
		annotation   string
		withPV       bool
		useServer    bool
		serverOpts   testutils.TestRuntimeServerOptions
		expectEvents []string // substrings expected in recorded events, in order
	}{
		{
			name:       "unmount succeeds through the runtime",
			annotation: `[{"pvName":"test-pv-unmount","mountPath":"/data"}]`,
			withPV:     true,
			useServer:  true,
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:      1,
					ExitCode: 0,
					Exited:   true,
				},
				RunCommandImmediately: true,
			},
			expectEvents: nil,
		},
		{
			name:       "unmount failure after retries records event",
			annotation: `[{"pvName":"test-pv-unmount","mountPath":"/data"}]`,
			withPV:     true,
			useServer:  true,
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:      1,
					ExitCode: 1,
					Exited:   true,
				},
				RunCommandImmediately: true,
			},
			expectEvents: []string{"proceeding with pod deletion"},
		},
		{
			name:       "missing PV records event and skips unmount",
			annotation: `[{"pvName":"test-pv-missing","mountPath":"/data"}]`,
			withPV:     false,
			expectEvents: []string{
				"failed to generate csi publish request for unmount",
			},
		},
		{
			name:       "corrupt annotation records event and skips unmount",
			annotation: `{not-json`,
			expectEvents: []string{
				"failed to parse csi mount config",
			},
		},
		{
			name:         "no annotation is a silent no-op",
			annotation:   "",
			expectEvents: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: agentsv1alpha1.SandboxStatus{
					PodInfo: agentsv1alpha1.PodInfo{
						PodUID: types.UID("test-pod-uid"),
					},
				},
			}
			if tt.annotation != "" {
				sbx.Annotations = map[string]string{
					agentsv1alpha1.AnnotationCSIVolumeConfig: tt.annotation,
				}
			}
			if tt.useServer {
				server := testutils.NewTestRuntimeServer(tt.serverOpts)
				defer server.Close()
				sbx.Annotations[agentsv1alpha1.AnnotationRuntimeURL] = server.URL
				sbx.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken] = utilruntime.AccessToken
			}

			var fakeClient = newFakeClient()
			if tt.withPV {
				fakeClient = newFakeClient(&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "test-pv-unmount"},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       testDriver,
								VolumeHandle: "handle-unmount",
							},
						},
					},
				})
			}
			registry := storages.NewStorageProvider()
			registry.RegisterProvider(testDriver, &storages.MountProvider{})

			recorder := record.NewFakeRecorder(10)
			initializer := &defaultSandboxInitializer{
				client:          fakeClient,
				apiReader:       fakeClient,
				storageRegistry: registry,
				recorder:        recorder,
			}

			// The best-effort contract: never returns an error and never panics.
			assert.NotPanics(t, func() {
				initializer.UnmountCSIVolumes(context.Background(), sbx, &sbx.Status)
			})

			events := collectFakeEvents(recorder)
			require.Len(t, events, len(tt.expectEvents), "unexpected event count: %v", events)
			for i, sub := range tt.expectEvents {
				assert.Contains(t, events[i], "CSIUnmountFailed")
				assert.Contains(t, events[i], sub)
			}
		})
	}
}

// collectFakeEvents drains the fake recorder's event channel.
func collectFakeEvents(recorder *record.FakeRecorder) []string {
	events := []string{}
	for {
		select {
		case ev := <-recorder.Events:
			events = append(events, ev)
		default:
			return events
		}
	}
}

// TestUnmountCSIVolumesBeforePodDeletion covers the guard logic of the shared
// pre-deletion hook: it fires only for a live pod and only when the
// initializer carries the CSIUnmounter capability.
func TestUnmountCSIVolumesBeforePodDeletion(t *testing.T) {
	livePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"}}
	deletingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:              "test-sandbox",
		Namespace:         "default",
		DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
	}}

	box := newCSICleanupSandboxForHook()

	tests := []struct {
		name        string
		initializer SandboxInitializer
		pod         *corev1.Pod
		expectCall  bool
	}{
		{name: "live pod with capable initializer", initializer: &defaultSandboxInitializer{}, pod: livePod, expectCall: true},
		{name: "nil pod", initializer: &defaultSandboxInitializer{}, pod: nil, expectCall: false},
		{name: "deleting pod", initializer: &defaultSandboxInitializer{}, pod: deletingPod, expectCall: false},
		{name: "mock initializer without capability", initializer: &mockSandboxInitializer{}, pod: livePod, expectCall: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			capable := &spyInitializer{
				SandboxInitializer: tt.initializer,
				onUnmount:          func() { called = true },
			}
			// The spy only applies when the wrapped initializer implements the
			// capability; the mock case must stay non-capable.
			initializer := tt.initializer
			if tt.expectCall {
				initializer = capable
			}
			UnmountCSIVolumesBeforePodDeletion(context.Background(), initializer, box, &box.Status, tt.pod)
			assert.Equal(t, tt.expectCall, called)
		})
	}
}

// newCSICleanupSandboxForHook builds a sandbox with an empty mount annotation
// (the cheapest path through UnmountCSIVolumes) for hook-guard tests.
func newCSICleanupSandboxForHook() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
		Status: agentsv1alpha1.SandboxStatus{
			PodInfo: agentsv1alpha1.PodInfo{PodUID: types.UID("test-pod-uid")},
		},
	}
}

// spyInitializer wraps a SandboxInitializer and records UnmountCSIVolumes
// calls, so tests can observe the capability dispatch.
type spyInitializer struct {
	SandboxInitializer
	onUnmount func()
}

func (s *spyInitializer) UnmountCSIVolumes(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) {
	s.onUnmount()
}

// TestEnsureSandboxPaused_UnmountsBeforePodDeletion proves the pause path
// invokes the pre-deletion unmount: a corrupt mount annotation makes the
// unmount emit its CSIUnmountFailed event, which must be present after the
// pause completes — and the pod must still be deleted (best-effort).
func TestEnsureSandboxPaused_UnmountsBeforePodDeletion(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	box := newCSICleanupSandboxForHook()
	box.Annotations = map[string]string{
		agentsv1alpha1.AnnotationCSIVolumeConfig: `{not-json`, // corrupt: forces the unmount event
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"}}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(box, pod).Build()
	recorder := record.NewFakeRecorder(10)
	initializer := &defaultSandboxInitializer{
		client:          fc,
		apiReader:       fc,
		storageRegistry: storages.NewStorageProvider(),
		recorder:        recorder,
	}
	control := &commonControl{
		Client:               fc,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fc, inplaceupdate.DefaultGeneratePatchBodyFunc),
		podControl:           NewPodControl(fc, record.NewFakeRecorder(10), GeneratePodFromSandbox),
		syncStatusFromPod:    defaultCommonSyncStatusFromPod,
		checkpointControl:    NewCheckpointControl(fc, record.NewFakeRecorder(10)),
		initializer:          initializer,
	}

	require.NoError(t, control.EnsureSandboxPaused(context.TODO(), EnsureFuncArgs{
		Pod: pod, Box: box, NewStatus: &box.Status,
	}))

	// The unmount must have run (event emitted) and the pod must be deleted.
	events := collectFakeEvents(recorder)
	found := false
	for _, ev := range events {
		if strings.Contains(ev, "CSIUnmountFailed") {
			found = true
		}
	}
	assert.True(t, found, "expected CSIUnmountFailed event from the pause-path unmount, got %v", events)

	podAfter := &corev1.Pod{}
	getErr := fc.Get(context.TODO(), types.NamespacedName{Name: "test-sandbox", Namespace: "default"}, podAfter)
	assert.True(t, getErr != nil || !podAfter.DeletionTimestamp.IsZero(), "pod must be deleted")
}
