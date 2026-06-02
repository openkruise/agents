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

package wake

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
)

const (
	SystemKeySecretName = keys.ConnectSystemKeySecretName
	SystemKeyDataKey    = keys.SystemKeyDataKey
)

type SystemKeyReader struct {
	Reader    client.Reader
	Namespace string
	Backoff   time.Duration
}

// WaitForKey blocks until the pre-created system-key Secret yields a non-empty
// key, then returns it. It is strictly read-only: it only ever issues Get and
// never creates or populates the Secret (only the manager populates it).
func (r *SystemKeyReader) WaitForKey(ctx context.Context) (string, error) {
	backoff := r.Backoff
	if backoff <= 0 {
		backoff = 5 * time.Second
	}
	log := klog.FromContext(ctx).WithValues("secret", SystemKeySecretName, "namespace", r.Namespace)
	for {
		key, err := r.readKey(ctx)
		if err == nil && strings.TrimSpace(key) != "" {
			return key, nil
		}
		if err != nil {
			log.Error(err, "system-key Secret is not ready; retrying")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
	}
}

func (r *SystemKeyReader) readKey(ctx context.Context) (string, error) {
	if r.Reader == nil {
		return "", fmt.Errorf("system-key reader is nil")
	}
	secret := &corev1.Secret{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: SystemKeySecretName}, secret); err != nil {
		return "", fmt.Errorf("get system-key secret: %w", err)
	}
	return string(secret.Data[SystemKeyDataKey]), nil
}
