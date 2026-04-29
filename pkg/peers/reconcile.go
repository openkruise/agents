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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/utils"
)

// reconcileInterval is a package-level variable for testability.
// Tests can override it to avoid waiting the full 60s between ticks.
var reconcileInterval = DefaultReconcileInterval

// RunPeerReconciliation starts a background goroutine that periodically lists
// peer pods from the Kubernetes API and reconciles them into the memberlist.
// This prevents split-brain scenarios when the initial memberlist join fails.
// If namespace or labelSelector is empty, the goroutine returns immediately.
func RunPeerReconciliation(ctx context.Context, client kubernetes.Interface, pm Peers,
	namespace, labelSelector, localIP string, bindPort int) {
	if namespace == "" || labelSelector == "" {
		return
	}

	log := klog.FromContext(ctx)
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			peerList, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
			})
			if err != nil {
				log.Error(err, "failed to list peer pods during reconciliation")
				continue
			}
			var peerAddrs []string
			for _, peer := range peerList.Items {
				ip := peer.Status.PodIP
				if ip == "" || ip == localIP || utils.IsLoopbackIP(ip) {
					continue
				}
				peerAddrs = append(peerAddrs, fmt.Sprintf("%s:%d", ip, bindPort))
			}
			if len(peerAddrs) > 0 {
				if err = pm.ReconcilePeers(ctx, peerAddrs); err != nil {
					log.V(4).Info("peer reconciliation completed with errors", "error", err)
				}
			}
		}
	}
}
