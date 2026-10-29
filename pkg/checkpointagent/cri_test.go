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
	"slices"
	"testing"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
		wantErr  bool
	}{
		{name: "unix url", endpoint: "unix:///run/containerd/containerd.sock", want: "unix:///run/containerd/containerd.sock"},
		{name: "absolute path", endpoint: "/run/containerd/containerd.sock", want: "unix:///run/containerd/containerd.sock"},
		{name: "empty", endpoint: "", wantErr: true},
		{name: "tcp endpoint", endpoint: "tcp://127.0.0.1:1234", wantErr: true},
		{name: "relative path", endpoint: "run/containerd.sock", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeEndpoint(tt.endpoint)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeEndpoint(%q) error = %v, wantErr %v", tt.endpoint, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("normalizeEndpoint(%q) = %q, want %q", tt.endpoint, got, tt.want)
			}
		})
	}
}

func TestResolvePodSandboxID(t *testing.T) {
	tests := []struct {
		name    string
		runtime *fakeRuntime
		want    string
		wantErr bool
	}{
		{
			name:    "exactly one ready sandbox",
			runtime: &fakeRuntime{sandboxes: []*runtimeapi.PodSandbox{{Id: testPodSandboxA}}},
			want:    testPodSandboxA,
		},
		{
			name:    "no ready sandbox",
			runtime: &fakeRuntime{},
			wantErr: true,
		},
		{
			// Two ready sandboxes for one Pod UID means the runtime is
			// mid-recreate and neither is safe to checkpoint.
			name:    "two ready sandboxes",
			runtime: &fakeRuntime{sandboxes: []*runtimeapi.PodSandbox{{Id: "sbx-1"}, {Id: "sbx-2"}}},
			wantErr: true,
		},
		{
			name:    "runtime rejects the list call",
			runtime: &fakeRuntime{listSandboxErr: errors.New("runtime unavailable")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePodSandboxID(context.TODO(), tt.runtime, "pod-uid-1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolvePodSandboxID() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("resolvePodSandboxID() = %q, want %q", got, tt.want)
			}
			// The filter is the only thing that ties a Pod object to the
			// sandbox the runtime knows about.
			if tt.runtime.listSandboxErr == nil && len(tt.runtime.sandboxReqs) == 1 {
				filter := tt.runtime.sandboxReqs[0].GetFilter()
				if state := filter.GetState().GetState(); state != runtimeapi.PodSandboxState_SANDBOX_READY {
					t.Errorf("sandbox filter state = %v, want SANDBOX_READY", state)
				}
				if uid := filter.GetLabelSelector()[criPodUIDLabel]; uid != "pod-uid-1" {
					t.Errorf("sandbox filter uid = %q, want %q", uid, "pod-uid-1")
				}
			}
		})
	}
}

func TestResolveContainerIDs(t *testing.T) {
	running := []*runtimeapi.Container{
		{Id: "ctr-main", Metadata: &runtimeapi.ContainerMetadata{Name: "main"}},
		{Id: "ctr-runtime", Metadata: &runtimeapi.ContainerMetadata{Name: "agent-runtime"}},
	}

	tests := []struct {
		name    string
		runtime *fakeRuntime
		names   []string
		want    []string
		wantErr bool
	}{
		{
			name:    "no selection returns every running container",
			runtime: &fakeRuntime{containers: running},
			want:    []string{"ctr-main", "ctr-runtime"},
		},
		{
			name:    "selection preserves the requested order",
			runtime: &fakeRuntime{containers: running},
			names:   []string{"agent-runtime", "main"},
			want:    []string{"ctr-runtime", "ctr-main"},
		},
		{
			// CheckpointPod requires the ID list to be exactly the selected
			// containers, so a partial match must not dump a subset.
			name:    "a name that is not running fails the whole selection",
			runtime: &fakeRuntime{containers: running},
			names:   []string{"main", "sidecar"},
			wantErr: true,
		},
		{
			name:    "no running container at all",
			runtime: &fakeRuntime{},
			wantErr: true,
		},
		{
			name:    "runtime rejects the list call",
			runtime: &fakeRuntime{listContainersErr: errors.New("runtime unavailable")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveContainerIDs(context.TODO(), tt.runtime, testPodSandboxA, tt.names)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveContainerIDs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(got, tt.want) {
				t.Errorf("resolveContainerIDs() = %v, want %v", got, tt.want)
			}
			if tt.runtime.listContainersErr == nil && len(tt.runtime.containerReqs) == 1 {
				filter := tt.runtime.containerReqs[0].GetFilter()
				if filter.GetPodSandboxId() != testPodSandboxA {
					t.Errorf("container filter sandbox = %q, want %q", filter.GetPodSandboxId(), testPodSandboxA)
				}
				if state := filter.GetState().GetState(); state != runtimeapi.ContainerState_CONTAINER_RUNNING {
					t.Errorf("container filter state = %v, want CONTAINER_RUNNING", state)
				}
			}
		})
	}
}

func TestDialRuntimeService(t *testing.T) {
	// The connection is lazy, so dialing a socket nobody listens on must still
	// succeed: a runtime that is not up yet cannot stop the agent from
	// starting.
	svc, conn, err := dialRuntimeService("unix:///tmp/kruise-agents-missing.sock")
	if err != nil {
		t.Fatalf("dialRuntimeService() error = %v", err)
	}
	if svc == nil {
		t.Error("expected a runtime service client")
	}
	if err := conn.Close(); err != nil {
		t.Errorf("failed to close the runtime connection: %v", err)
	}

	if _, _, err := dialRuntimeService("tcp://127.0.0.1:1234"); err == nil {
		t.Error("expected a non-unix endpoint to be rejected")
	}
}

func TestCloseOnStop(t *testing.T) {
	closed := false
	ctx, cancel := context.WithCancel(context.TODO())
	cancel()
	if err := (closeOnStop{closer: funcCloser(func() error { closed = true; return nil })}).Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !closed {
		t.Error("expected the runtime connection to be closed when the manager stops")
	}
}

type funcCloser func() error

func (f funcCloser) Close() error { return f() }
