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

package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
)

func TestUnifiedBackendLifecycleAndSubjectIsolation(t *testing.T) {
	var mu sync.Mutex
	var requests []unifiedQuotaPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing unified quota bearer")
		}
		var payload unifiedQuotaPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		requests = append(requests, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	backend, err := NewUnifiedBackend(server.URL, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	subject := Subject{OrganizationID: "org", OwnerID: "team", SandboxType: "container", TemplateID: "tpl", Enabled: true}
	manager := NewManager(NoopBackend{})
	manager.SetUnifiedBackend(backend)
	if err := manager.Acquire(t.Context(), AcquireRequest{User: "key-a", LockString: "lock-1", Subject: subject}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unknown(t.Context(), UnknownRequest{User: "key-a", LockString: "lock-1", Subject: subject}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(t.Context(), ReleaseRequest{User: "key-a", LockString: "lock-1", Subject: subject}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 || requests[0].TemplateID != "tpl" || requests[1].TemplateID != "" || requests[2].Proof == "" {
		t.Fatalf("unexpected unified lifecycle payloads: %#v", requests)
	}
}

func TestUnifiedBackendMapsQuotaExceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	backend, err := NewUnifiedBackend(server.URL, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = backend.AcquireSubject(t.Context(), Subject{OrganizationID: "org", OwnerID: "team", SandboxType: "container", Enabled: true}, "lock")
	if err != ErrQuotaExceeded {
		t.Fatalf("expected quota exceeded, got %v", err)
	}
}

func TestManagerRollsBackLegacyAcquireWhenUnifiedRejects(t *testing.T) {
	legacy := &recordingBackend{acquireErr: nil}
	manager := NewManager(legacy)
	manager.SetUnifiedBackend(&recordingSubjectBackend{err: ErrQuotaExceeded})
	spec := &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeAll, Limit: 1}}}
	err := manager.Acquire(t.Context(), AcquireRequest{User: "key", LockString: "lock", Quota: spec, Subject: Subject{OrganizationID: "org", OwnerID: "team", SandboxType: "container", Enabled: true}})
	if err != ErrQuotaExceeded || legacy.releases != 1 {
		t.Fatalf("expected unified rejection with legacy rollback, err=%v releases=%d", err, legacy.releases)
	}
}

type recordingSubjectBackend struct{ err error }

func (b *recordingSubjectBackend) AcquireSubject(context.Context, Subject, string) error {
	return b.err
}
func (b *recordingSubjectBackend) ReleaseSubject(context.Context, Subject, string) error { return nil }
func (b *recordingSubjectBackend) UnknownSubject(context.Context, Subject, string) error { return nil }

type recordingBackend struct {
	acquireErr error
	releases   int
}

func (b *recordingBackend) Acquire(context.Context, AcquireParams) error  { return b.acquireErr }
func (b *recordingBackend) Release(context.Context, string, string) error { b.releases++; return nil }
func (b *recordingBackend) ListEntries(context.Context, string) (map[string]Entry, error) {
	return map[string]Entry{}, nil
}
func (b *recordingBackend) Cleanup(context.Context, string) error { return nil }
