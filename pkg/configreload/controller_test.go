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

package configreload

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestDispatcherConvergence scripts one outcome per key call. The sent values
// show which intents are retried; releasedFrom pins the snapshot that first
// releases startup.
func TestDispatcherConvergence(t *testing.T) {
	type step struct {
		values  map[string]string
		outcome string // key component result; empty means no call
	}
	for _, tc := range []struct {
		name         string
		steps        []step
		want         []string
		releasedFrom int // 1-based snapshot after which startup must be released
	}{
		{
			name:         "failed write is retried",
			releasedFrom: 1,
			steps: []step{
				{values: map[string]string{"key": "A", "other": "0"}, outcome: "ok"},
				{values: map[string]string{"key": "B", "other": "0"}, outcome: "error"},
				{values: map[string]string{"key": "B", "other": "0"}, outcome: "ok"},
			},
			want: []string{"A", "B", "B"},
		},
		{
			name:         "uncertain write returns to confirmed",
			releasedFrom: 1,
			steps: []step{
				{values: map[string]string{"key": "A", "other": "0"}, outcome: "ok"},
				{values: map[string]string{"key": "B", "other": "0"}, outcome: "error"},
				{values: map[string]string{"key": "A", "other": "0"}, outcome: "ok"},
			},
			want: []string{"A", "B", "A"},
		},
		{
			name:         "rejection confirms nothing and leaves nothing pending",
			releasedFrom: 1,
			steps: []step{
				{values: map[string]string{"key": "A", "other": "0"}, outcome: "ok"},
				{values: map[string]string{"key": "B", "other": "0"}, outcome: "rejected"},
				{values: map[string]string{"key": "A", "other": "0"}},
			},
			want: []string{"A", "B"},
		},
		{
			name:         "rejection preserves an uncertain write before returning to confirmed",
			releasedFrom: 1,
			steps: []step{
				{values: map[string]string{"key": "A", "other": "0"}, outcome: "ok"},
				{values: map[string]string{"key": "B", "other": "0"}, outcome: "error"},
				{values: map[string]string{"key": "C", "other": "0"}, outcome: "rejected"},
				{values: map[string]string{"key": "A", "other": "0"}, outcome: "ok"},
			},
			want: []string{"A", "B", "C", "A"},
		},
		{
			name:         "initial rejection holds startup",
			releasedFrom: 2,
			steps: []step{
				{values: map[string]string{"key": "A", "other": "0"}, outcome: "rejected"},
				{values: map[string]string{"key": "A", "other": "0"}, outcome: "ok"},
				{values: map[string]string{"key": "B", "other": "0"}, outcome: "error"},
				{values: map[string]string{"key": "C", "other": "0"}, outcome: "ok"},
			},
			want: []string{"A", "A", "B", "C"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outcomes []string
			for _, s := range tc.steps {
				if s.outcome != "" {
					outcomes = append(outcomes, s.outcome)
				}
			}
			var sent []string
			otherCalls := 0
			d := NewDispatcher([]Component{
				{Name: "key", Keys: []string{"key"}, Reload: func(_ context.Context, changes map[string]string) (Result, error) {
					require.Less(t, len(sent), len(outcomes), "reload without a scripted outcome")
					outcome := outcomes[len(sent)]
					sent = append(sent, changes["key"])
					switch outcome {
					case "error":
						return "", errors.New("backend unavailable")
					case "rejected":
						return Rejected, nil
					}
					return Accepted, nil
				}},
				{Name: "other", Keys: []string{"other"}, Reload: func(context.Context, map[string]string) (Result, error) { otherCalls++; return Accepted, nil }},
			})
			for i, s := range tc.steps {
				err := d.Apply(t.Context(), s.values)
				require.Equal(t, s.outcome == "error", err != nil, "Apply reports failed effects only")
				require.Equal(t, 1, otherCalls, "a failure must not stop a confirmed component")
				select {
				case <-d.Initialized():
					require.GreaterOrEqual(t, i+1, tc.releasedFrom, "released startup before every key was confirmed")
				default:
					require.Less(t, i+1, tc.releasedFrom, "confirmed keys did not release startup")
				}
			}
			require.Equal(t, tc.want, sent)
		})
	}
}

func TestSecretReconcilerPreservesState(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cfg"}, Data: map[string][]byte{"key": []byte("A")}}
	c := fake.NewClientBuilder().WithObjects(secret).Build()
	calls := 0
	d := NewDispatcher([]Component{{Name: "key", Keys: []string{"key"}, Reload: func(context.Context, map[string]string) (Result, error) { calls++; return Accepted, nil }}})
	r := secretReconciler{reader: c, ref: types.NamespacedName{Namespace: "ns", Name: "cfg"}, dispatcher: d, parse: func(data map[string][]byte) (map[string]string, error) {
		v, ok := data["key"]
		if !ok {
			return nil, errors.New("missing key")
		}
		return map[string]string{"key": string(v)}, nil
	}}
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	require.NoError(t, c.Delete(t.Context(), secret))
	_, err = r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	secret.ResourceVersion = ""
	secret.Data = map[string][]byte{}
	require.NoError(t, c.Create(t.Context(), secret))
	_, err = r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}

// Exercise the manager's real queue and reconcile context: calling Reload with
// a test logger directly would miss controller-runtime replacing that logger.
func TestWatcherReconcileLogs(t *testing.T) {
	logs := make(chan string, 128)
	ctx, cancel := context.WithCancel(klog.NewContext(t.Context(), funcr.New(func(_, line string) { logs <- line }, funcr.Options{})))
	defer cancel()
	secret := corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cfg", ResourceVersion: "1"},
		Data:       map[string][]byte{"key": []byte("sensitive-input")},
	}
	updates := make(chan watch.Event)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var response any
		switch r.URL.Path {
		case "/api":
			response = metav1.APIVersions{Versions: []string{"v1"}}
		case "/apis":
			response = metav1.APIGroupList{}
		case "/api/v1":
			response = metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: metav1.Verbs{"get", "list", "watch"}}}}
		case "/api/v1/namespaces/ns/secrets":
			if selector := r.URL.Query().Get("fieldSelector"); selector != "metadata.name=cfg" {
				t.Errorf("Secret LIST/WATCH fieldSelector = %q, want metadata.name=cfg", selector)
			}
			if r.URL.Query().Get("sendInitialEvents") == "true" {
				w.WriteHeader(http.StatusBadRequest)
				response = metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonBadRequest, Code: 400}
			} else if r.URL.Query().Get("watch") == "true" {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				for {
					select {
					case event := <-updates:
						if err := json.NewEncoder(w).Encode(map[string]any{"type": event.Type, "object": event.Object}); err != nil {
							return
						}
						w.(http.Flusher).Flush()
					case <-r.Context().Done():
						return
					}
				}
			} else {
				response = corev1.SecretList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []corev1.Secret{secret}}
			}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("write API fixture: %v", err)
		}
	}))
	defer server.Close()
	reconciled := make(chan struct{}, 1)
	d := NewDispatcher([]Component{{Name: "key", Keys: []string{"key"}, Reload: func(ctx context.Context, changes map[string]string) (Result, error) {
		select {
		case reconciled <- struct{}{}:
		default:
		}
		switch changes["key"] {
		case "accepted":
			return Accepted, nil
		case "incomplete":
			return "", errors.New("sensitive-backend-detail")
		default:
			klog.FromContext(ctx).Info("identity change requires restart")
			return Rejected, nil
		}
	}}})
	parse := func(data map[string][]byte) (map[string]string, error) {
		if len(data["key"]) == 0 {
			return nil, errors.New("missing key")
		}
		return map[string]string{"key": string(data["key"])}, nil
	}
	done := make(chan error, 1)
	go func() {
		done <- RunWatcher(ctx, &rest.Config{Host: server.URL}, types.NamespacedName{Namespace: "ns", Name: "cfg"}, parse, d)
	}()
	defer func() { cancel(); require.ErrorIs(t, <-done, context.Canceled) }()
	select {
	case <-reconciled:
	case <-time.After(3 * time.Second):
		t.Fatal("manager did not reconcile the initial Secret")
	}
	waitForLog := func(message string) {
		t.Helper()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for {
			select {
			case line := <-logs:
				require.NotContains(t, line, "sensitive-input")
				require.NotContains(t, line, "sensitive-backend-detail")
				if strings.Contains(line, message) {
					require.Contains(t, line, `"controller"="runtime-config"`)
					require.Contains(t, line, "reconcileID")
					return
				}
			case <-timer.C:
				t.Fatalf("missing log from real reconcile context: %s", message)
			}
		}
	}
	waitForLog("identity change requires restart")
	waitForLog("configuration rejected")
	for i, step := range []struct{ value, message string }{
		{"accepted", "configuration applied"},
		{"incomplete", "configuration reload incomplete"},
		{"", "configuration Secret has invalid shape"},
		{"deleted", "configuration Secret is absent"},
	} {
		next := secret.DeepCopy()
		next.ResourceVersion = strconv.Itoa(i + 2)
		next.Data["key"] = []byte(step.value)
		event := watch.Event{Type: watch.Modified, Object: next}
		if step.value == "deleted" {
			event.Type = watch.Deleted
		}
		select {
		case updates <- event:
		case <-time.After(3 * time.Second):
			t.Fatal("watch update was not consumed")
		}
		waitForLog(step.message)
	}
}

func TestRunWatcherRetriesUntilCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := make(chan string, 2)
		ctx, cancel := context.WithCancel(klog.NewContext(t.Context(), funcr.New(func(_, line string) {
			logs <- line
		}, funcr.Options{})))
		defer cancel()
		done := make(chan error, 1)
		go func() {
			// A malformed host rejects construction before any network access.
			done <- RunWatcher(ctx, &rest.Config{Host: "://invalid-sensitive-host"}, types.NamespacedName{Namespace: "ns", Name: "cfg"}, nil, NewDispatcher(nil))
		}()
		started := time.Now()
		for range 2 {
			line := <-logs
			require.Contains(t, line, "configuration observer reconnecting")
			require.NotContains(t, line, "invalid-sensitive-host")
		}
		require.Equal(t, time.Second, time.Since(started), "failed observer construction must back off before retrying")
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}
