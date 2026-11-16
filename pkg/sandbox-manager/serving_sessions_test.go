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

package sandbox_manager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	session "github.com/openkruise/agents/pkg/sessionservice"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
)

func managerSessionFixture(t *testing.T) (*SandboxManager, client.Client, *agents.SessionService) {
	t.Helper()
	parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "serving", UID: "serving-uid", Labels: map[string]string{sessionbinding.SessionServiceIDLabel: "serving-id"}}, Spec: agents.SessionServiceSpec{Ports: []int32{8080}}}
	box := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "member", UID: "member-uid", Labels: map[string]string{sessionbinding.SessionServiceUIDLabel: string(parent.UID)}, Annotations: map[string]string{sessionbinding.BindingsAnnotation: `{"000000000000000000000Z":{"lastRequestAt":"2026-11-10T00:00:00Z"},"000000000000000000000a":{"lastRequestAt":"2026-11-10T00:00:00Z"},"000000000000000000000b":{"lastRequestAt":"2026-11-10T00:00:00Z"}}`}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, agents.GroupVersion.WithKind("SessionService"))}}}
	c, writer, err := cachetest.NewTestSessionServiceCache(t, parent, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	backend := sandboxcr.NewInfraBuilder(config.InitOptions(config.SandboxManagerOptions{})).WithCache(c).Build()
	m := &SandboxManager{infra: backend}
	return m, writer, parent
}

func requireSessionError(t *testing.T, err error, code managererrors.ErrorCode, message string) {
	t.Helper()
	var typed *managererrors.Error
	if !errors.As(err, &typed) || typed.Code != code || typed.Message != message {
		t.Fatalf("error %v does not carry code %s and message %q", err, code, message)
	}
}

func TestSessionServiceManagerSessionsAndPagination(t *testing.T) {
	m, c, parent := managerSessionFixture(t)
	got, err := m.GetSessionServiceSession(t.Context(), "team", "serving-id", "000000000000000000000a")
	if err != nil || got.SessionID != "000000000000000000000a" || got.SandboxID != "team--member" {
		t.Fatalf("incorrect durable session: %+v %v", got, err)
	}
	_, err = m.GetSessionServiceSession(t.Context(), "00000000000000000other", "serving-id", "000000000000000000000a")
	requireSessionError(t, err, managererrors.ErrorNotFound, "SessionService not found")
	_, err = m.GetSessionServiceSession(t.Context(), "team", "serving-id", "000000000000000missing")
	requireSessionError(t, err, managererrors.ErrorNotFound, "Session not found")
	for _, id := range []string{"", " a", "a.b", "中文"} {
		_, err = m.GetSessionServiceSession(t.Context(), "team", "serving-id", id)
		requireSessionError(t, err, managererrors.ErrorBadRequest, "Invalid session ID")
	}
	limit := 1
	page, err := m.ListSessionServiceSessions(t.Context(), "team", "serving-id", ListSessionServiceSessionsOptions{Limit: &limit})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].SessionID != "000000000000000000000Z" || page.NextToken == "" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	second, err := m.ListSessionServiceSessions(t.Context(), "team", "serving-id", ListSessionServiceSessionsOptions{Limit: &limit, ContinueToken: page.NextToken})
	if err != nil || len(second.Sessions) != 1 || second.Sessions[0].SessionID != "000000000000000000000a" {
		t.Fatalf("second page: %+v %v", second, err)
	}
	for _, tt := range []struct{ token, message string }{
		{"invalid", "Invalid session cursor"},
		{page.NextToken + "\n", "Invalid session cursor"},
		{base64.RawURLEncoding.EncodeToString([]byte(`{"uid":"00000000000000000other","after":"000000000000000000000Z"}`)), "Invalid or stale session cursor"},
		{base64.RawURLEncoding.EncodeToString([]byte(`{"uid":"wrong","uid":"serving-uid","after":"000000000000000000000Z"}`)), "Invalid or stale session cursor"},
	} {
		_, err = m.ListSessionServiceSessions(t.Context(), "team", "serving-id", ListSessionServiceSessionsOptions{ContinueToken: tt.token})
		requireSessionError(t, err, managererrors.ErrorBadRequest, tt.message)
	}
	for _, invalid := range []int{0, -1, 101} {
		_, err = m.ListSessionServiceSessions(t.Context(), "team", "serving-id", ListSessionServiceSessionsOptions{Limit: &invalid})
		requireSessionError(t, err, managererrors.ErrorBadRequest, "Limit must be between 1 and 100")
	}
	result, err := m.CloseSessionServiceSession(t.Context(), "team", "serving-id", "000000000000000000000a")
	if err != nil || result.SessionID != "000000000000000000000a" {
		t.Fatalf("close: %+v %v", result, err)
	}
	if _, err := m.CloseSessionServiceSession(t.Context(), "team", "serving-id", "000000000000000000000a"); err != nil {
		t.Fatal("Close not idempotent:", err)
	}
	_, err = m.GetSessionServiceSession(t.Context(), "team", "serving-id", "000000000000000000000a")
	requireSessionError(t, err, managererrors.ErrorNotFound, "Session not found")
	if err := c.Delete(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	parent.ResourceVersion = ""
	parent.UID = "new-uid"
	if err := c.Create(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	_, err = m.ListSessionServiceSessions(t.Context(), "team", "serving-id", ListSessionServiceSessionsOptions{ContinueToken: page.NextToken})
	requireSessionError(t, err, managererrors.ErrorBadRequest, "Invalid or stale session cursor")
}

func TestSessionServiceManagerMissingBackendUnavailable(t *testing.T) {
	var m SandboxManager
	if err := m.initializeSessionServiceRuntime(t.Context()); err != nil || m.generateSessionServiceID != nil {
		t.Fatalf("absent optional backend started identity coordination: %v", err)
	}
	_, err := m.GetSessionServiceSession(t.Context(), "team", "id", "000000000000000000000a")
	requireSessionError(t, err, managererrors.ErrorUnavailable, "Session manager is unavailable")
}

func TestSessionServiceWorkerLeaseDomain(t *testing.T) {
	m, c, _ := managerSessionFixture(t)
	m.systemNamespace = "sandbox-system"
	if err := m.initializeSessionServiceRuntime(t.Context()); err != nil {
		t.Fatal(err)
	}
	var servingLease coordinationv1.Lease
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: "sandbox-system", Name: "sandbox-manager-session-service-id-worker"}, &servingLease))
	require.EqualValues(t, 0, *servingLease.Spec.LeaseTransitions)
	worker, err := m.allocateWorkerID(t.Context(), "serving-")
	require.NoError(t, err)
	require.EqualValues(t, 0, worker, "Sandbox prefix domain must remain independent")
	var sandboxLease coordinationv1.Lease
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: "sandbox-system", Name: workerLeaseName("serving-")}, &sandboxLease))
	require.NotEqual(t, servingLease.Name, sandboxLease.Name)
}

func TestSessionServiceManagerIdentityBootstrap(t *testing.T) {
	m, c, _ := managerSessionFixture(t)
	for i := 0; i < 34; i++ {
		p := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: fmt.Sprint("new-", i), UID: types.UID(fmt.Sprint("new-", i))}}
		if err := c.Create(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	m.systemNamespace = "sandbox-system"
	if err := m.initializeSessionServiceRuntime(t.Context()); err != nil {
		t.Fatal(err)
	}
	backend := m.sessionBackend()
	if err := m.assignMissingSessionServiceIDs(t.Context(), backend); err != nil {
		t.Fatal(err)
	}
	first, err := backend.ListSessionServices(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	ids := map[string]bool{}
	for _, parent := range first {
		if parent.ID != "" {
			count++
			if ids[parent.ID] {
				t.Fatal("duplicate SessionService identity")
			}
			ids[parent.ID] = true
			if parent.Name != "serving" {
				require.Regexp(t, `^[a-z2-7]{13}$`, parent.ID)
			}
		}
	}
	if count != 33 {
		t.Fatalf("identity bootstrap did not bound the first batch: %d", count)
	}
	if err := m.assignMissingSessionServiceIDs(t.Context(), backend); err != nil {
		t.Fatal(err)
	}
	for _, parent := range first {
		if parent.ID != "" {
			got, err := backend.GetSessionService(context.Background(), parent.Namespace, parent.ID)
			if err != nil || got.ID != parent.ID {
				t.Fatal("stable identity changed on rescan")
			}
		}
	}
	var stored agents.SessionServiceList
	if err := c.List(t.Context(), &stored); err != nil {
		t.Fatal(err)
	}
	for _, parent := range stored.Items {
		if parent.Labels[sessionbinding.SessionServiceIDLabel] == "" {
			t.Fatal("new SessionService never received identity")
		}
	}
}

func TestSessionServiceManagerPaginationBounds(t *testing.T) {
	m, c, _ := managerSessionFixture(t)
	var box agents.Sandbox
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: "member"}, &box); err != nil {
		t.Fatal(err)
	}
	records := make(map[string]map[string]string)
	for i := 0; i < 120; i++ {
		records[fmt.Sprintf("session_%014d", i)] = map[string]string{"lastRequestAt": "2026-11-10T00:00:00Z"}
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	box.Annotations[sessionbinding.BindingsAnnotation] = string(raw)
	if err := c.Update(t.Context(), &box); err != nil {
		t.Fatal(err)
	}
	max := 100
	for _, tt := range []struct {
		name  string
		limit *int
		want  int
	}{{"default", nil, 50}, {"maximum", &max, 100}} {
		t.Run(tt.name, func(t *testing.T) {
			page, err := m.ListSessionServiceSessions(t.Context(), "team", "serving-id", ListSessionServiceSessionsOptions{Limit: tt.limit})
			if err != nil || len(page.Sessions) != tt.want || page.NextToken == "" {
				t.Fatalf("page bounds: %d %v", len(page.Sessions), err)
			}
		})
	}
}

// Override only cached client observations to expose deterministic write cuts;
// every identity operation still uses the production SandboxCR implementation.
type sessionServiceIdentityTestCache struct {
	infracache.Provider
	infracache.SessionServiceCache
	observed client.Client
}

func (c sessionServiceIdentityTestCache) GetClient() client.Client { return c.observed }

func managerSessionServiceIDBackend(t *testing.T, parent *agents.SessionService, hooks interceptor.Funcs) (session.SessionServiceSessionBackend, client.Client) {
	t.Helper()
	cache, writer, err := cachetest.NewTestSessionServiceCache(t, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	watched, ok := writer.(client.WithWatch)
	if !ok {
		t.Fatal("test cache client does not support existing interceptors")
	}
	observed := sessionServiceIdentityTestCache{Provider: cache, SessionServiceCache: cache.(infracache.SessionServiceCache), observed: interceptor.NewClient(watched, hooks)}
	implementation := sandboxcr.NewInfraBuilder(config.InitOptions(config.SandboxManagerOptions{})).WithCache(observed).Build()
	backend := implementation.(session.SessionServiceSessionProvider).GetSessionServiceSessionBackend()
	if backend == nil {
		t.Fatal("test SessionService backend unavailable")
	}
	return backend, writer
}

func TestSessionServiceManagerConcurrentIdentityAssignment(t *testing.T) {
	// Two independent managers must not overwrite one another after both read
	// the same unlabeled parent. Releasing both real conditional patches together
	// makes the losing assignment exercise Manager's conflict branch.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var successful, conflicted atomic.Int32
	parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "concurrent", UID: "concurrent-uid", Labels: map[string]string{"example.com/keep": "yes"}}, Spec: agents.SessionServiceSpec{Ports: []int32{8080}}}
	backend, writer := managerSessionServiceIDBackend(t, parent, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			select {
			case entered <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			err := c.Patch(ctx, obj, patch, opts...)
			if err == nil {
				successful.Add(1)
			} else if apierrors.IsConflict(err) {
				conflicted.Add(1)
			}
			return err
		},
	})
	first := &SandboxManager{servingBackend: backend, generateSessionServiceID: func() (string, error) { return "first", nil }}
	second := &SandboxManager{servingBackend: backend, generateSessionServiceID: func() (string, error) { return "second", nil }}
	done := make(chan error, 2)
	for _, manager := range []*SandboxManager{first, second} {
		go func() { done <- manager.assignMissingSessionServiceIDs(ctx, backend) }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("both assignments did not reach the shared stale-version cut:", ctx.Err())
		}
	}
	close(release)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("Manager failed to tolerate identity assignment conflict:", err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent assignments did not finish:", ctx.Err())
		}
	}
	var stored agents.SessionService
	if err := writer.Get(ctx, client.ObjectKeyFromObject(parent), &stored); err != nil {
		t.Fatal(err)
	}
	winner := stored.Labels[sessionbinding.SessionServiceIDLabel]
	if winner != "first" && winner != "second" {
		t.Fatalf("unexpected persisted identity %q", winner)
	}
	if successful.Load() != 1 || conflicted.Load() != 1 {
		t.Fatalf("conditional assignments: success=%d conflict=%d", successful.Load(), conflicted.Load())
	}
	if stored.UID != parent.UID || stored.Labels["example.com/keep"] != "yes" || len(stored.Spec.Ports) != 1 || stored.Spec.Ports[0] != 8080 {
		t.Fatalf("identity race changed parent content: %+v", stored)
	}
	for _, manager := range []*SandboxManager{first, second} {
		manager.generateSessionServiceID = func() (string, error) { return "", errors.New("stable identity must not regenerate") }
		if err := manager.assignMissingSessionServiceIDs(ctx, backend); err != nil {
			t.Fatal(err)
		}
		found, err := manager.GetSessionService(ctx, "team", winner)
		if err != nil || found.UID != string(parent.UID) || found.ID != winner {
			t.Fatalf("manager did not resolve the same persisted identity: %+v %v", found, err)
		}
	}
	if successful.Load() != 1 || conflicted.Load() != 1 {
		t.Fatal("rescan attempted to overwrite the established identity")
	}
}

func TestSessionServiceManagerIdentityAssignmentAfterStaleObservation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		loseACK bool
	}{
		{name: "acknowledged write and old index"},
		{name: "lost write acknowledgement and old index", loseACK: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "lagged", UID: "lagged-uid"}}
			var stale agents.SessionService
			lag := false
			patches, successful, conflicted := 0, 0, 0
			backend, writer := managerSessionServiceIDBackend(t, parent, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if target, ok := obj.(*agents.SessionService); ok && lag {
						*target = *stale.DeepCopy()
						return nil
					}
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if parents, ok := list.(*agents.SessionServiceList); ok && lag {
						listOpts := (&client.ListOptions{}).ApplyOptions(opts)
						parents.Items = nil
						// The cached exact-ID index is still based on the unlabeled object.
						if (listOpts.Namespace == "" || listOpts.Namespace == stale.Namespace) && (listOpts.FieldSelector == nil || listOpts.FieldSelector.Matches(fields.Set{infracache.IndexSessionServiceID: stale.Labels[sessionbinding.SessionServiceIDLabel]})) {
							parents.Items = []agents.SessionService{*stale.DeepCopy()}
						}
						return nil
					}
					return c.List(ctx, list, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					err := c.Patch(ctx, obj, patch, opts...)
					if err != nil {
						if apierrors.IsConflict(err) {
							conflicted++
						}
						return err
					}
					successful++
					if tt.loseACK && patches == 1 {
						return context.DeadlineExceeded
					}
					return nil
				},
			})
			if err := writer.Get(t.Context(), client.ObjectKeyFromObject(parent), &stale); err != nil {
				t.Fatal(err)
			}
			lag = true
			generated := 0
			manager := &SandboxManager{servingBackend: backend, generateSessionServiceID: func() (string, error) { generated++; return fmt.Sprint("attempt-", generated), nil }}
			err := manager.assignMissingSessionServiceIDs(t.Context(), backend)
			if tt.loseACK {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lost acknowledgement was not surfaced: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var committed agents.SessionService
			if err := writer.Get(t.Context(), client.ObjectKeyFromObject(parent), &committed); err != nil {
				t.Fatal(err)
			}
			if committed.Labels[sessionbinding.SessionServiceIDLabel] != "attempt-1" {
				t.Fatalf("first assignment was not persisted: %+v", committed.Labels)
			}
			if _, err := manager.GetSessionService(t.Context(), "team", "attempt-1"); err == nil {
				t.Fatal("old cached index unexpectedly observed the new identity")
			} else {
				requireSessionError(t, err, managererrors.ErrorNotFound, "SessionService not found")
			}
			// A second scan still sees the original UID/RV and proposes another ID;
			// the production conditional patch must preserve the already committed ID.
			if err := manager.assignMissingSessionServiceIDs(t.Context(), backend); err != nil {
				t.Fatal("stale scan conflict was not tolerated:", err)
			}
			var afterRetry agents.SessionService
			if err := writer.Get(t.Context(), client.ObjectKeyFromObject(parent), &afterRetry); err != nil {
				t.Fatal(err)
			}
			if afterRetry.Labels[sessionbinding.SessionServiceIDLabel] != "attempt-1" || afterRetry.ResourceVersion != committed.ResourceVersion {
				t.Fatalf("stale scan replaced the committed identity: %+v", afterRetry)
			}
			if patches != 2 || successful != 1 || conflicted != 1 || generated != 2 {
				t.Fatalf("write cuts not exercised: patches=%d success=%d conflict=%d generated=%d", patches, successful, conflicted, generated)
			}
			lag = false
			if err := manager.assignMissingSessionServiceIDs(t.Context(), backend); err != nil {
				t.Fatal(err)
			}
			found, err := manager.GetSessionService(t.Context(), "team", "attempt-1")
			if err != nil || found.ID != "attempt-1" || found.UID != "lagged-uid" {
				t.Fatalf("fresh index did not restore stable lookup: %+v %v", found, err)
			}
			if generated != 2 || patches != 2 {
				t.Fatal("fresh rescan regenerated or rewrote an established identity")
			}
			_, err = manager.GetSessionService(t.Context(), "team", "attempt-2")
			requireSessionError(t, err, managererrors.ErrorNotFound, "SessionService not found")
		})
	}
}

// assignMissingSessionServiceIDs drives individual assignment attempts in persistence
// tests. Production assignment is exclusively driven by the event worker.
func (m *SandboxManager) assignMissingSessionServiceIDs(ctx context.Context, backend session.SessionServiceSessionBackend) error {
	parents, err := backend.ListSessionServices(ctx)
	if err != nil {
		return err
	}
	source := backend.(session.SessionServiceIdentitySource)
	attempted := 0
	for _, parent := range parents {
		if parent.ID != "" || parent.Deleting {
			continue
		}
		if attempted == 32 {
			break
		}
		attempted++
		err := m.assignSessionServiceIdentity(ctx, backend, source, sessionServiceIdentityKey{parent.Namespace, parent.Name, parent.UID})
		if errors.Is(err, session.ErrSessionServiceConflict) || errors.Is(err, session.ErrSessionServiceNotFound) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
