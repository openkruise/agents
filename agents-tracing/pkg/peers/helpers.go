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

package peers

import (
	"context"
	"fmt"
	"net"

	"github.com/openkruise/agents/api/v1alpha1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LabelSelectorKey   = "agents.kruise.io/peer"
	LabelSelectorValue = "true"
	LabelSelector      = LabelSelectorKey + "=" + LabelSelectorValue
	Namespace          = "default"
)

// getFreePort returns an available local port
func getFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

// CreateTestPeer creates a test MemberlistPeers instance. Any failure is
// returned as an error so that callers (typically tests) can decide how to
// surface it (e.g. via testify require/assert).
func CreateTestPeer(ctx context.Context, c client.Client, nodeName string) (*MemberlistPeers, int, error) {
	port, err := getFreePort()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get free port: %w", err)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeName,
			Namespace: Namespace,
			Labels: map[string]string{
				LabelSelectorKey: LabelSelectorValue,
			},
			Annotations: map[string]string{
				v1alpha1.AnnotationMemberlistURL: fmt.Sprintf("127.0.0.1:%d", port),
			},
		},
		Status: v1.PodStatus{
			PodIP: "127.0.0.1",
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		return nil, 0, fmt.Errorf("failed to create peer pod: %w", err)
	}

	peer := NewMemberlistPeers(c, nodeName, Namespace, LabelSelector)
	peer.localIP = "127.0.0.1"
	return peer, port, nil
}
