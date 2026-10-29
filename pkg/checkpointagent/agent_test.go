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

package checkpointagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	testNodeName    = "node-a"
	testOtherNode   = "node-b"
	testRuntimeCls  = "kata-checkpoint"
	testCheckpointA = "cp-checkpoint-uid-1"
	testPodSandboxA = "sbx-1"
	// testArtifact is a file a dump left in the checkpoint directory.
	testArtifact = "manifest.json"
)

// fakeRuntime stands in for the node's container runtime. It ignores the CRI
// filters and returns canned objects, so the requests it records are the only
// place the filter contents can be asserted.
type fakeRuntime struct {
	sandboxes  []*runtimeapi.PodSandbox
	containers []*runtimeapi.Container

	listSandboxErr    error
	listContainersErr error
	checkpointErr     error

	sandboxReqs    []*runtimeapi.ListPodSandboxRequest
	containerReqs  []*runtimeapi.ListContainersRequest
	checkpointReqs []*runtimeapi.CheckpointPodRequest
}

func (f *fakeRuntime) ListPodSandbox(_ context.Context, in *runtimeapi.ListPodSandboxRequest, _ ...grpc.CallOption) (*runtimeapi.ListPodSandboxResponse, error) {
	f.sandboxReqs = append(f.sandboxReqs, in)
	if f.listSandboxErr != nil {
		return nil, f.listSandboxErr
	}
	return &runtimeapi.ListPodSandboxResponse{Items: f.sandboxes}, nil
}

func (f *fakeRuntime) ListContainers(_ context.Context, in *runtimeapi.ListContainersRequest, _ ...grpc.CallOption) (*runtimeapi.ListContainersResponse, error) {
	f.containerReqs = append(f.containerReqs, in)
	if f.listContainersErr != nil {
		return nil, f.listContainersErr
	}
	return &runtimeapi.ListContainersResponse{Containers: f.containers}, nil
}

func (f *fakeRuntime) CheckpointPod(_ context.Context, in *runtimeapi.CheckpointPodRequest, _ ...grpc.CallOption) (*runtimeapi.CheckpointPodResponse, error) {
	f.checkpointReqs = append(f.checkpointReqs, in)
	if f.checkpointErr != nil {
		return nil, f.checkpointErr
	}
	return &runtimeapi.CheckpointPodResponse{}, nil
}

// readyRuntime is a runtime holding one ready sandbox with two running
// containers: "main" from the sandbox template and "agent-runtime" injected at
// pod creation.
func readyRuntime() *fakeRuntime {
	return &fakeRuntime{
		sandboxes: []*runtimeapi.PodSandbox{
			{Id: testPodSandboxA, State: runtimeapi.PodSandboxState_SANDBOX_READY},
		},
		containers: []*runtimeapi.Container{
			{Id: "ctr-main", PodSandboxId: testPodSandboxA, Metadata: &runtimeapi.ContainerMetadata{Name: "main"}},
			{Id: "ctr-runtime", PodSandboxId: testPodSandboxA, Metadata: &runtimeapi.ContainerMetadata{Name: "agent-runtime"}},
		},
	}
}

func agentTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add client-go scheme: %v", err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add agents scheme: %v", err)
	}
	return scheme
}

// newAgentTestSandbox declares only the "main" container, so a pod carrying
// "agent-runtime" as well produces a non-empty pod template delta.
func newAgentTestSandbox() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default", UID: "sandbox-uid-1"},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "nginx:1.21"}},
					},
				},
			},
		},
	}
}

func newAgentTestPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default", UID: "pod-uid-1"},
		Spec: corev1.PodSpec{
			NodeName:         testNodeName,
			RuntimeClassName: ptr.To(testRuntimeCls),
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx:1.21"},
				{Name: "agent-runtime", Image: "agent-runtime:v1"},
			},
		},
	}
}

func newAgentTestCheckpoint(contents ...string) *agentsv1alpha1.Checkpoint {
	return &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-checkpoint",
			Namespace: "default",
			UID:       "checkpoint-uid-1",
		},
		Spec: agentsv1alpha1.CheckpointSpec{
			SandboxName:        ptr.To("test-sandbox"),
			PersistentContents: contents,
		},
	}
}

// localSource is the status location an agent publishes when it claims a
// checkpoint for node.
func localSource(node, path string) *agentsv1alpha1.CheckpointSource {
	return &agentsv1alpha1.CheckpointSource{
		Type:  agentsv1alpha1.CheckpointSourceTypeLocal,
		Local: &agentsv1alpha1.LocalCheckpointSource{NodeName: node, Path: path},
	}
}

func TestReconcile(t *testing.T) {
	scheme := agentTestScheme(t)

	// creating returns a checkpoint already claimed by node, which is the
	// state executeCheckpoint starts from.
	creating := func(node string) *agentsv1alpha1.Checkpoint {
		cp := newAgentTestCheckpoint(agentsv1alpha1.CheckpointPersistentContentMemory)
		cp.Finalizers = []string{agentsv1alpha1.CheckpointFinalizer}
		cp.Status.Phase = agentsv1alpha1.CheckpointCreating
		cp.Status.CheckpointId = testCheckpointA
		cp.Status.CheckpointLocation = localSource(node, filepath.Join("/does-not-matter", testCheckpointA))
		return cp
	}

	tests := []struct {
		name string
		cp   *agentsv1alpha1.Checkpoint
		// podOnNode controls whether the target pod exists in the cache; the
		// cache is restricted to this node, so absent means "not ours".
		podOnNode bool
		// mutatePod adjusts the pod before it is seeded.
		mutatePod func(*corev1.Pod)
		// sandboxMissing drops the owning Sandbox, which must not fail a
		// completed dump.
		sandboxMissing bool
		// allRuntimeClasses leaves Options.RuntimeClasses empty, making every
		// pod on the node eligible.
		allRuntimeClasses bool
		// seedDir pre-creates the checkpoint directory holding an artifact,
		// standing in for what a previous dump wrote. Whenever the directory
		// is expected to exist afterwards, the artifact must have survived.
		seedDir bool
		// rememberedDump seeds the outcome of a dump this process already
		// ran, which is the state a stale reconcile finds.
		rememberedDump *dumpOutcome
		// rootIsFile replaces the checkpoint root with a regular file, which is
		// what a misconfigured host path mount looks like to the agent.
		rootIsFile bool
		runtime    *fakeRuntime

		wantErr             bool
		wantPhase           agentsv1alpha1.CheckpointPhase
		wantLocationNode    string
		wantCheckpointID    string
		wantFinalizer       bool
		wantDeltaRecorded   bool
		wantCheckpointCalls int
		wantDirExists       bool
		// wantRemembered expects the dump outcome to still be remembered,
		// which holds until a terminal or deleting Checkpoint is observed.
		wantRemembered bool
		// wantGone expects the checkpoint object itself to be collected, which
		// only happens once the last finalizer is released.
		wantGone bool
	}{
		{
			name:      "pod is not on this node, nothing is claimed",
			cp:        newAgentTestCheckpoint(agentsv1alpha1.CheckpointPersistentContentMemory),
			podOnNode: false,
			runtime:   readyRuntime(),
			wantPhase: "",
		},
		{
			name:      "checkpoint without a target is not claimed",
			cp:        &agentsv1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: "test-checkpoint", Namespace: "default", UID: "checkpoint-uid-1"}},
			podOnNode: true,
			runtime:   readyRuntime(),
			wantPhase: "",
		},
		{
			name:      "pod runtime class is not checkpointable",
			cp:        newAgentTestCheckpoint(agentsv1alpha1.CheckpointPersistentContentMemory),
			podOnNode: true,
			mutatePod: func(pod *corev1.Pod) { pod.Spec.RuntimeClassName = ptr.To("runc") },
			runtime:   readyRuntime(),
			wantPhase: "",
		},
		{
			name:      "pod without a runtime class is not checkpointable when classes are restricted",
			cp:        newAgentTestCheckpoint(agentsv1alpha1.CheckpointPersistentContentMemory),
			podOnNode: true,
			mutatePod: func(pod *corev1.Pod) { pod.Spec.RuntimeClassName = nil },
			runtime:   readyRuntime(),
			wantPhase: "",
		},
		{
			name:              "empty runtime class list makes every pod eligible",
			cp:                newAgentTestCheckpoint(agentsv1alpha1.CheckpointPersistentContentMemory),
			podOnNode:         true,
			mutatePod:         func(pod *corev1.Pod) { pod.Spec.RuntimeClassName = nil },
			allRuntimeClasses: true,
			runtime:           readyRuntime(),
			wantPhase:         agentsv1alpha1.CheckpointCreating,
			wantLocationNode:  testNodeName,
			wantCheckpointID:  testCheckpointA,
		},
		{
			name:             "fresh checkpoint is claimed with a node-local location",
			cp:               newAgentTestCheckpoint(agentsv1alpha1.CheckpointPersistentContentMemory),
			podOnNode:        true,
			runtime:          readyRuntime(),
			wantPhase:        agentsv1alpha1.CheckpointCreating,
			wantLocationNode: testNodeName,
			wantCheckpointID: testCheckpointA,
			// Claiming publishes the destination before dumping anything.
			wantCheckpointCalls: 0,
			wantDirExists:       false,
		},
		{
			name: "pod-info-only checkpoint succeeds without dumping",
			cp: func() *agentsv1alpha1.Checkpoint {
				return newAgentTestCheckpoint(agentsv1alpha1.CheckpointPersistentContentPodInfo)
			}(),
			podOnNode:         true,
			runtime:           readyRuntime(),
			wantPhase:         agentsv1alpha1.CheckpointSucceeded,
			wantDeltaRecorded: true,
			// No artifacts, so no finalizer is taken and no directory exists.
			wantFinalizer: false,
			wantDirExists: false,
		},
		{
			name:                "claimed checkpoint is dumped and recorded",
			cp:                  creating(testNodeName),
			podOnNode:           true,
			runtime:             readyRuntime(),
			wantPhase:           agentsv1alpha1.CheckpointSucceeded,
			wantCheckpointID:    testCheckpointA,
			wantLocationNode:    testNodeName,
			wantFinalizer:       true,
			wantDeltaRecorded:   true,
			wantCheckpointCalls: 1,
			wantDirExists:       true,
			wantRemembered:      true,
		},
		{
			// The first execution is the one that takes the finalizer, so it is
			// also the one whose terminal status update has to survive the
			// resource version the finalizer patch bumped.
			name: "first execution takes the finalizer and still records the outcome",
			cp: func() *agentsv1alpha1.Checkpoint {
				cp := creating(testNodeName)
				cp.Finalizers = nil
				return cp
			}(),
			podOnNode:           true,
			runtime:             readyRuntime(),
			wantPhase:           agentsv1alpha1.CheckpointSucceeded,
			wantCheckpointID:    testCheckpointA,
			wantLocationNode:    testNodeName,
			wantFinalizer:       true,
			wantDeltaRecorded:   true,
			wantCheckpointCalls: 1,
			wantDirExists:       true,
			wantRemembered:      true,
		},
		{
			name:                "dump succeeds even when the sandbox is already gone",
			cp:                  creating(testNodeName),
			podOnNode:           true,
			sandboxMissing:      true,
			runtime:             readyRuntime(),
			wantPhase:           agentsv1alpha1.CheckpointSucceeded,
			wantCheckpointID:    testCheckpointA,
			wantLocationNode:    testNodeName,
			wantFinalizer:       true,
			wantDeltaRecorded:   false,
			wantCheckpointCalls: 1,
			wantDirExists:       true,
			wantRemembered:      true,
		},
		{
			// The success update raced the next reconcile, which still reads
			// Creating after the sandbox pause already stopped the pod. The
			// artifacts must survive and the success be recorded again.
			name:                "dump already run in this process is recorded, not repeated",
			cp:                  creating(testNodeName),
			podOnNode:           false,
			seedDir:             true,
			rememberedDump:      &dumpOutcome{},
			runtime:             readyRuntime(),
			wantPhase:           agentsv1alpha1.CheckpointSucceeded,
			wantCheckpointID:    testCheckpointA,
			wantLocationNode:    testNodeName,
			wantFinalizer:       true,
			wantCheckpointCalls: 0,
			wantDirExists:       true,
			wantRemembered:      true,
		},
		{
			name:                "dump that already failed in this process is recorded, not repeated",
			cp:                  creating(testNodeName),
			podOnNode:           true,
			rememberedDump:      &dumpOutcome{err: errors.New("CheckpointPod failed: boom")},
			runtime:             readyRuntime(),
			wantPhase:           agentsv1alpha1.CheckpointFailed,
			wantCheckpointID:    testCheckpointA,
			wantLocationNode:    testNodeName,
			wantFinalizer:       true,
			wantCheckpointCalls: 0,
			wantRemembered:      true,
		},
		{
			name:                "dump fails when the target pod left the node",
			cp:                  creating(testNodeName),
			podOnNode:           false,
			runtime:             readyRuntime(),
			wantPhase:           agentsv1alpha1.CheckpointFailed,
			wantLocationNode:    testNodeName,
			wantCheckpointID:    testCheckpointA,
			wantFinalizer:       true,
			wantCheckpointCalls: 0,
		},
		{
			name:      "dump fails when the runtime has no ready sandbox",
			cp:        creating(testNodeName),
			podOnNode: true,
			runtime:   &fakeRuntime{},
			wantPhase: agentsv1alpha1.CheckpointFailed,
			// The directory is created before the runtime is queried and is
			// only reclaimed when the dump itself fails.
			wantDirExists:       true,
			wantLocationNode:    testNodeName,
			wantCheckpointID:    testCheckpointA,
			wantFinalizer:       true,
			wantCheckpointCalls: 0,
		},
		{
			name:      "failed dump reclaims its directory",
			cp:        creating(testNodeName),
			podOnNode: true,
			runtime: func() *fakeRuntime {
				f := readyRuntime()
				f.checkpointErr = errors.New("runtime does not implement CheckpointPod")
				return f
			}(),
			wantPhase:           agentsv1alpha1.CheckpointFailed,
			wantLocationNode:    testNodeName,
			wantCheckpointID:    testCheckpointA,
			wantFinalizer:       true,
			wantCheckpointCalls: 1,
			wantDirExists:       false,
			wantRemembered:      true,
		},
		{
			name: "checkpoint id that escapes the root is rejected",
			cp: func() *agentsv1alpha1.Checkpoint {
				cp := creating(testNodeName)
				cp.Status.CheckpointId = "../escaped"
				return cp
			}(),
			podOnNode:           true,
			runtime:             readyRuntime(),
			wantPhase:           agentsv1alpha1.CheckpointFailed,
			wantLocationNode:    testNodeName,
			wantCheckpointID:    "../escaped",
			wantFinalizer:       true,
			wantCheckpointCalls: 0,
		},
		{
			name:                "dump fails when the checkpoint root is not a directory",
			cp:                  creating(testNodeName),
			podOnNode:           true,
			rootIsFile:          true,
			runtime:             readyRuntime(),
			wantPhase:           agentsv1alpha1.CheckpointFailed,
			wantLocationNode:    testNodeName,
			wantCheckpointID:    testCheckpointA,
			wantFinalizer:       true,
			wantCheckpointCalls: 0,
		},
		{
			name:             "checkpoint claimed by another node is left alone",
			cp:               creating(testOtherNode),
			podOnNode:        true,
			runtime:          readyRuntime(),
			wantPhase:        agentsv1alpha1.CheckpointCreating,
			wantLocationNode: testOtherNode,
			wantCheckpointID: testCheckpointA,
			wantFinalizer:    true,
		},
		{
			name: "terminal checkpoint keeps its artifacts",
			cp: func() *agentsv1alpha1.Checkpoint {
				cp := creating(testNodeName)
				cp.Status.Phase = agentsv1alpha1.CheckpointSucceeded
				return cp
			}(),
			podOnNode:        true,
			seedDir:          true,
			rememberedDump:   &dumpOutcome{},
			runtime:          readyRuntime(),
			wantPhase:        agentsv1alpha1.CheckpointSucceeded,
			wantLocationNode: testNodeName,
			wantCheckpointID: testCheckpointA,
			wantFinalizer:    true,
			wantDirExists:    true,
		},
		{
			name: "deleted checkpoint releases its artifacts and finalizer",
			cp: func() *agentsv1alpha1.Checkpoint {
				cp := creating(testNodeName)
				cp.Status.Phase = agentsv1alpha1.CheckpointSucceeded
				cp.DeletionTimestamp = ptr.To(metav1.Now())
				return cp
			}(),
			podOnNode:      true,
			seedDir:        true,
			rememberedDump: &dumpOutcome{},
			runtime:        readyRuntime(),
			wantDirExists:  false,
			wantGone:       true,
		},
		{
			name: "deleted checkpoint owned by another node keeps its finalizer",
			cp: func() *agentsv1alpha1.Checkpoint {
				cp := creating(testOtherNode)
				cp.Status.Phase = agentsv1alpha1.CheckpointSucceeded
				cp.DeletionTimestamp = ptr.To(metav1.Now())
				return cp
			}(),
			podOnNode: true,
			seedDir:   true,
			runtime:   readyRuntime(),
			wantPhase: agentsv1alpha1.CheckpointSucceeded,
			// The other node's agent holds the same finalizer and owns both
			// the artifacts and their removal.
			wantLocationNode: testOtherNode,
			wantCheckpointID: testCheckpointA,
			wantFinalizer:    true,
			wantDirExists:    true,
		},
		{
			name: "deleted checkpoint with an unusable id still releases the finalizer",
			cp: func() *agentsv1alpha1.Checkpoint {
				cp := creating(testNodeName)
				cp.Status.Phase = agentsv1alpha1.CheckpointFailed
				cp.Status.CheckpointId = "../escaped"
				cp.DeletionTimestamp = ptr.To(metav1.Now())
				return cp
			}(),
			podOnNode: true,
			runtime:   readyRuntime(),
			wantGone:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.rootIsFile {
				root = filepath.Join(root, "not-a-directory")
				if err := os.WriteFile(root, nil, 0o600); err != nil {
					t.Fatalf("failed to seed the checkpoint root: %v", err)
				}
			}
			objects := []client.Object{tt.cp}
			if !tt.sandboxMissing {
				objects = append(objects, newAgentTestSandbox())
			}
			if tt.podOnNode {
				pod := newAgentTestPod()
				if tt.mutatePod != nil {
					tt.mutatePod(pod)
				}
				objects = append(objects, pod)
			}
			if tt.seedDir {
				if err := os.MkdirAll(filepath.Join(root, testCheckpointA), 0o700); err != nil {
					t.Fatalf("failed to seed checkpoint directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(root, testCheckpointA, testArtifact), nil, 0o600); err != nil {
					t.Fatalf("failed to seed checkpoint artifact: %v", err)
				}
			}

			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
				WithStatusSubresource(&agentsv1alpha1.Checkpoint{}).Build()
			opts := Options{
				NodeName:          testNodeName,
				CheckpointRoot:    root,
				CheckpointTimeout: DefaultCheckpointTimeout,
			}
			if !tt.allRuntimeClasses {
				opts.RuntimeClasses = []string{testRuntimeCls}
			}
			r := &Reconciler{Client: fc, runtime: tt.runtime, opts: opts}
			if tt.rememberedDump != nil {
				r.dumps.Store(tt.cp.UID, *tt.rememberedDump)
			}

			key := types.NamespacedName{Namespace: tt.cp.Namespace, Name: tt.cp.Name}
			_, err := r.Reconcile(context.TODO(), ctrl.Request{NamespacedName: key})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile() error = %v, wantErr %v", err, tt.wantErr)
			}

			got := &agentsv1alpha1.Checkpoint{}
			getErr := fc.Get(context.TODO(), key, got)
			if tt.wantGone {
				if getErr == nil {
					t.Errorf("expected checkpoint to be collected, finalizers = %v", got.Finalizers)
				}
			} else {
				if getErr != nil {
					t.Fatalf("failed to get checkpoint: %v", getErr)
				}
				if got.Status.Phase != tt.wantPhase {
					t.Errorf("phase = %q, want %q (message %q)", got.Status.Phase, tt.wantPhase, got.Status.Message)
				}
				if got.Status.CheckpointId != tt.wantCheckpointID {
					t.Errorf("checkpointId = %q, want %q", got.Status.CheckpointId, tt.wantCheckpointID)
				}
				gotNode := ""
				if loc := got.Status.CheckpointLocation; loc != nil {
					if loc.Type != agentsv1alpha1.CheckpointSourceTypeLocal || loc.Local == nil {
						t.Fatalf("checkpointLocation = %+v, want a local source", loc)
					}
					gotNode = loc.Local.NodeName
				}
				if gotNode != tt.wantLocationNode {
					t.Errorf("checkpointLocation node = %q, want %q", gotNode, tt.wantLocationNode)
				}
				if has := controllerutil.ContainsFinalizer(got, agentsv1alpha1.CheckpointFinalizer); has != tt.wantFinalizer {
					t.Errorf("has finalizer = %v, want %v", has, tt.wantFinalizer)
				}
				if has := len(got.Status.PodTemplateDelta.Raw) > 0; has != tt.wantDeltaRecorded {
					t.Errorf("pod template delta recorded = %v, want %v", has, tt.wantDeltaRecorded)
				}
				if tt.wantPhase == agentsv1alpha1.CheckpointFailed && got.Status.Message == "" {
					t.Error("expected a failure message on the checkpoint status")
				}
			}

			if calls := len(tt.runtime.checkpointReqs); calls != tt.wantCheckpointCalls {
				t.Errorf("CheckpointPod calls = %d, want %d", calls, tt.wantCheckpointCalls)
			}
			if _, statErr := os.Stat(filepath.Join(root, testCheckpointA)); (statErr == nil) != tt.wantDirExists {
				t.Errorf("checkpoint directory exists = %v, want %v", statErr == nil, tt.wantDirExists)
			}
			if tt.seedDir && tt.wantDirExists {
				if _, statErr := os.Stat(filepath.Join(root, testCheckpointA, testArtifact)); statErr != nil {
					t.Errorf("expected the seeded artifact to survive, stat error = %v", statErr)
				}
			}
			if _, remembered := r.dumps.Load(tt.cp.UID); remembered != tt.wantRemembered {
				t.Errorf("dump outcome remembered = %v, want %v", remembered, tt.wantRemembered)
			}
		})
	}
}

// TestReconcileDeletingWithoutFinalizer covers the one deleting branch the
// table cannot reach: the fake client refuses to hold an object that carries a
// deletion timestamp without a finalizer.
func TestReconcileDeletingWithoutFinalizer(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, testCheckpointA), 0o700); err != nil {
		t.Fatalf("failed to seed checkpoint directory: %v", err)
	}
	cp := newAgentTestCheckpoint(agentsv1alpha1.CheckpointPersistentContentMemory)
	cp.DeletionTimestamp = ptr.To(metav1.Now())
	cp.Status.CheckpointId = testCheckpointA
	cp.Status.CheckpointLocation = localSource(testNodeName, filepath.Join(root, testCheckpointA))

	r := &Reconciler{
		Client:  fake.NewClientBuilder().WithScheme(agentTestScheme(t)).Build(),
		runtime: readyRuntime(),
		opts:    Options{NodeName: testNodeName, CheckpointRoot: root},
	}
	if err := r.reconcileDeleting(context.TODO(), cp); err != nil {
		t.Fatalf("reconcileDeleting() error = %v", err)
	}
	// Without the finalizer the artifacts were never this agent's to reclaim.
	if _, err := os.Stat(filepath.Join(root, testCheckpointA)); err != nil {
		t.Errorf("expected the checkpoint directory to be left alone, stat error = %v", err)
	}
}

// TestCheckpointPodRequest asserts the CRI calls a dump issues: the pod
// sandbox is looked up by Pod UID, and CheckpointPod receives exactly the
// requested containers plus the directory published in the status.
func TestCheckpointPodRequest(t *testing.T) {
	tests := []struct {
		name           string
		specContainers []string
		wantIDs        []string
	}{
		{
			name:    "no container selection dumps every running container",
			wantIDs: []string{"ctr-main", "ctr-runtime"},
		},
		{
			name:           "explicit selection dumps only the named containers",
			specContainers: []string{"main"},
			wantIDs:        []string{"ctr-main"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cp := newAgentTestCheckpoint(agentsv1alpha1.CheckpointPersistentContentMemory)
			cp.Spec.Containers = tt.specContainers
			cp.Finalizers = []string{agentsv1alpha1.CheckpointFinalizer}
			cp.Status.Phase = agentsv1alpha1.CheckpointCreating
			cp.Status.CheckpointId = testCheckpointA
			cp.Status.CheckpointLocation = localSource(testNodeName, filepath.Join(root, testCheckpointA))

			fc := fake.NewClientBuilder().WithScheme(agentTestScheme(t)).
				WithObjects(cp, newAgentTestSandbox(), newAgentTestPod()).
				WithStatusSubresource(&agentsv1alpha1.Checkpoint{}).Build()
			fr := readyRuntime()
			r := &Reconciler{
				Client:  fc,
				runtime: fr,
				opts: Options{
					NodeName:          testNodeName,
					CheckpointRoot:    root,
					RuntimeClasses:    []string{testRuntimeCls},
					CheckpointTimeout: DefaultCheckpointTimeout,
				},
			}

			key := types.NamespacedName{Namespace: cp.Namespace, Name: cp.Name}
			if _, err := r.Reconcile(context.TODO(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			if len(fr.sandboxReqs) != 1 {
				t.Fatalf("ListPodSandbox calls = %d, want 1", len(fr.sandboxReqs))
			}
			if uid := fr.sandboxReqs[0].GetFilter().GetLabelSelector()[criPodUIDLabel]; uid != "pod-uid-1" {
				t.Errorf("pod sandbox filter uid = %q, want %q", uid, "pod-uid-1")
			}
			if len(fr.checkpointReqs) != 1 {
				t.Fatalf("CheckpointPod calls = %d, want 1", len(fr.checkpointReqs))
			}
			req := fr.checkpointReqs[0]
			if req.GetPodSandboxId() != testPodSandboxA {
				t.Errorf("podSandboxId = %q, want %q", req.GetPodSandboxId(), testPodSandboxA)
			}
			if want := filepath.Join(root, testCheckpointA); req.GetOutputPath() != want {
				t.Errorf("outputPath = %q, want %q", req.GetOutputPath(), want)
			}
			if got := req.GetContainerIds(); !slices.Equal(got, tt.wantIDs) {
				t.Errorf("containerIds = %v, want %v", got, tt.wantIDs)
			}
			// CheckpointPod requires an existing empty directory it owns.
			if entries, err := os.ReadDir(req.GetOutputPath()); err != nil {
				t.Errorf("output path is not a directory: %v", err)
			} else if len(entries) != 0 {
				t.Errorf("output path is not empty: %d entries", len(entries))
			}
		})
	}
}

// TestReconcileTargetsPodName checks the Pod-named target, which the Sandbox
// controller does not use today but the API allows.
func TestReconcileTargetsPodName(t *testing.T) {
	root := t.TempDir()
	cp := newAgentTestCheckpoint(agentsv1alpha1.CheckpointPersistentContentMemory)
	cp.Spec.SandboxName = nil
	cp.Spec.PodName = ptr.To("test-sandbox")

	fc := fake.NewClientBuilder().WithScheme(agentTestScheme(t)).
		WithObjects(cp, newAgentTestSandbox(), newAgentTestPod()).
		WithStatusSubresource(&agentsv1alpha1.Checkpoint{}).Build()
	r := &Reconciler{
		Client:  fc,
		runtime: readyRuntime(),
		opts:    Options{NodeName: testNodeName, CheckpointRoot: root, RuntimeClasses: []string{testRuntimeCls}},
	}
	key := types.NamespacedName{Namespace: cp.Namespace, Name: cp.Name}
	if _, err := r.Reconcile(context.TODO(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := &agentsv1alpha1.Checkpoint{}
	if err := fc.Get(context.TODO(), key, got); err != nil {
		t.Fatalf("failed to get checkpoint: %v", err)
	}
	if got.Status.Phase != agentsv1alpha1.CheckpointCreating {
		t.Errorf("phase = %q, want %q", got.Status.Phase, agentsv1alpha1.CheckpointCreating)
	}
}

func TestReconcileMissingCheckpoint(t *testing.T) {
	r := &Reconciler{
		Client:  fake.NewClientBuilder().WithScheme(agentTestScheme(t)).Build(),
		runtime: readyRuntime(),
		opts:    Options{NodeName: testNodeName, CheckpointRoot: t.TempDir()},
	}
	key := types.NamespacedName{Namespace: "default", Name: "gone"}
	if _, err := r.Reconcile(context.TODO(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Errorf("Reconcile() error = %v, want nil for a deleted checkpoint", err)
	}
}

func TestCheckpointPath(t *testing.T) {
	r := &Reconciler{opts: Options{CheckpointRoot: "/var/lib/checkpoints"}}
	tests := []struct {
		name         string
		checkpointID string
		want         string
		wantErr      bool
	}{
		{name: "plain id", checkpointID: "cp-uid-1", want: "/var/lib/checkpoints/cp-uid-1"},
		{name: "empty id", checkpointID: "", wantErr: true},
		{name: "parent traversal", checkpointID: "..", wantErr: true},
		{name: "current directory", checkpointID: ".", wantErr: true},
		{name: "relative escape", checkpointID: "../../etc", wantErr: true},
		{name: "absolute path", checkpointID: "/etc/passwd", wantErr: true},
		{name: "nested path", checkpointID: "a/b", wantErr: true},
		{name: "trailing separator", checkpointID: "cp-uid-1/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.checkpointPath(tt.checkpointID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkpointPath(%q) error = %v, wantErr %v", tt.checkpointID, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("checkpointPath(%q) = %q, want %q", tt.checkpointID, got, tt.want)
			}
		})
	}
}

func TestAddRejectsMissingNodeName(t *testing.T) {
	// Add dials the runtime and registers with a manager, so only the
	// precondition that needs no node is exercised here.
	if err := Add(nil, Options{}); err == nil {
		t.Error("expected Add to reject an empty node name")
	}
}
