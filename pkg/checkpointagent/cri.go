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
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// criPodUIDLabel is the label every CRI implementation stamps on a pod sandbox
// with the Kubernetes Pod UID. Selecting on it is what maps a Pod object to
// the sandbox the runtime knows, without having to parse pod sandbox metadata.
const criPodUIDLabel = "io.kubernetes.pod.uid"

// runtimeService is the subset of the CRI runtime service the agent calls.
// Only these three RPCs are needed to checkpoint a pod, and narrowing the
// surface is what makes the reconciler testable without a container runtime.
type runtimeService interface {
	ListPodSandbox(ctx context.Context, in *runtimeapi.ListPodSandboxRequest, opts ...grpc.CallOption) (*runtimeapi.ListPodSandboxResponse, error)
	ListContainers(ctx context.Context, in *runtimeapi.ListContainersRequest, opts ...grpc.CallOption) (*runtimeapi.ListContainersResponse, error)
	CheckpointPod(ctx context.Context, in *runtimeapi.CheckpointPodRequest, opts ...grpc.CallOption) (*runtimeapi.CheckpointPodResponse, error)
}

// dialRuntimeService connects to the CRI runtime service at endpoint, which
// must be a unix socket path or a unix:// URL. The connection is lazy: it is
// established on the first RPC, so a runtime that is not yet up at agent
// startup does not prevent the agent from starting.
func dialRuntimeService(endpoint string) (runtimeapi.RuntimeServiceClient, *grpc.ClientConn, error) {
	target, err := normalizeEndpoint(endpoint)
	if err != nil {
		return nil, nil, err
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to CRI endpoint %q: %w", endpoint, err)
	}
	return runtimeapi.NewRuntimeServiceClient(conn), conn, nil
}

// normalizeEndpoint turns a CRI endpoint into a gRPC target. Only unix
// sockets are accepted: the agent runs on the node next to the runtime, and a
// remote runtime endpoint would mean the checkpoint directory it writes is not
// the directory the agent reports in the Checkpoint status.
func normalizeEndpoint(endpoint string) (string, error) {
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		return endpoint, nil
	case strings.HasPrefix(endpoint, "/"):
		return "unix://" + endpoint, nil
	default:
		return "", fmt.Errorf("unsupported CRI endpoint %q: expected a unix socket path or unix:// URL", endpoint)
	}
}

// resolvePodSandboxID returns the CRI pod sandbox ID of the ready sandbox
// belonging to the given Pod UID.
func resolvePodSandboxID(ctx context.Context, svc runtimeService, podUID string) (string, error) {
	resp, err := svc.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{
		Filter: &runtimeapi.PodSandboxFilter{
			State:         &runtimeapi.PodSandboxStateValue{State: runtimeapi.PodSandboxState_SANDBOX_READY},
			LabelSelector: map[string]string{criPodUIDLabel: podUID},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pod sandboxes: %w", err)
	}
	if len(resp.GetItems()) == 0 {
		return "", fmt.Errorf("no ready pod sandbox found for pod uid %s", podUID)
	}
	if len(resp.GetItems()) > 1 {
		// Two ready sandboxes for one Pod UID means the runtime is mid-recreate
		// and picking either one could checkpoint the sandbox that is about to
		// go away. Fail and let the next reconcile find a settled state.
		return "", fmt.Errorf("found %d ready pod sandboxes for pod uid %s, expected exactly one", len(resp.GetItems()), podUID)
	}
	return resp.GetItems()[0].GetId(), nil
}

// resolveContainerIDs returns the IDs of the running containers of the given
// pod sandbox. When names is non-empty only those containers are selected, and
// every requested name must resolve to a running container: CheckpointPod
// requires the ID list to be exactly the containers the caller selected, so a
// partial match has to fail rather than silently checkpoint a subset.
func resolveContainerIDs(ctx context.Context, svc runtimeService, sandboxID string, names []string) ([]string, error) {
	resp, err := svc.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{
			PodSandboxId: sandboxID,
			State:        &runtimeapi.ContainerStateValue{State: runtimeapi.ContainerState_CONTAINER_RUNNING},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers of sandbox %s: %w", sandboxID, err)
	}

	running := make(map[string]string, len(resp.GetContainers()))
	ids := make([]string, 0, len(resp.GetContainers()))
	for _, c := range resp.GetContainers() {
		running[c.GetMetadata().GetName()] = c.GetId()
		ids = append(ids, c.GetId())
	}

	if len(names) == 0 {
		if len(ids) == 0 {
			return nil, fmt.Errorf("no running container found in sandbox %s", sandboxID)
		}
		return ids, nil
	}

	selected := make([]string, 0, len(names))
	for _, name := range names {
		id, ok := running[name]
		if !ok {
			return nil, fmt.Errorf("container %q is not running in sandbox %s", name, sandboxID)
		}
		selected = append(selected, id)
	}
	return selected, nil
}
