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

package delegation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/pkg/identity/aead"
)

const storeNamespace = "sandbox-system"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	return s
}

func testKeyring(t *testing.T) *aead.Keyring {
	t.Helper()
	keyring, err := aead.NewKeyring(aead.Key{
		ID:     "2026-11",
		Secret: bytes.Repeat([]byte{0x11}, aead.KeySize),
	})
	if err != nil {
		t.Fatalf("build keyring: %v", err)
	}
	return keyring
}

func newStores(t *testing.T, objects ...client.Object) (*SecretSessionStore, *SecretDelegationStore, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	keyring := testKeyring(t)

	sessions, err := NewSecretSessionStore(c, keyring, storeNamespace)
	if err != nil {
		t.Fatalf("NewSecretSessionStore: %v", err)
	}
	delegations, err := NewSecretDelegationStore(c, keyring, storeNamespace)
	if err != nil {
		t.Fatalf("NewSecretDelegationStore: %v", err)
	}
	return sessions, delegations, c
}

func testSessionKey() SessionKey {
	return SessionKey{Namespace: "team-a", StateDigest: "ZGlnZXN0LW9mLXRoZS1zdGF0ZQ"}
}

func testSession(now time.Time) *Session {
	return &Session{
		Key:                testSessionKey(),
		Verifier:           "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		AgentIdentity:      "checkout-agent",
		SandboxName:        "sbx-1",
		SandboxUID:         "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		UserIssuer:         "https://idp.example.com",
		UserSubject:        "alice@example.com",
		CredentialProvider: "github",
		Scopes:             []string{"repo:status"},
		RedirectURI:        "https://agents.example.com/oauth2/callback",
		CreatedAt:          now,
		ExpiresAt:          now.Add(10 * time.Minute),
		Status:             StatusPending,
	}
}

func testDelegationKey() DelegationKey {
	return DelegationKey{
		Namespace:          "team-a",
		UserIssuer:         "https://idp.example.com",
		UserSubject:        "alice@example.com",
		AgentIdentity:      "checkout-agent",
		CredentialProvider: "github",
	}
}

func testDelegation(now time.Time) *Delegation {
	return &Delegation{
		Key:                  testDelegationKey(),
		AccessToken:          "gho_theAccessToken",
		RefreshToken:         "ghr_theRefreshToken",
		ExternalAccountID:    "octocat",
		Scopes:               []string{"repo:status"},
		AccessTokenExpiresAt: now.Add(time.Hour),
		State:                DelegationActive,
		UpdatedAt:            now,
	}
}

// TestSessionRoundTrip covers the ordinary path and checks that everything a
// callback must restore rather than accept survives storage.
func TestSessionRoundTrip(t *testing.T) {
	sessions, _, _ := newStores(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	session := testSession(now)
	if err := sessions.Create(ctx, session); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if session.Version == "" {
		t.Error("Create did not record a version")
	}

	got, err := sessions.Get(ctx, testSessionKey())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// §6.4: these are restored from the session, never read from the callback.
	for _, field := range []struct{ name, got, want string }{
		{"verifier", got.Verifier, session.Verifier},
		{"agentIdentity", got.AgentIdentity, session.AgentIdentity},
		{"sandboxName", got.SandboxName, session.SandboxName},
		{"sandboxUid", got.SandboxUID, session.SandboxUID},
		{"userIssuer", got.UserIssuer, session.UserIssuer},
		{"userSubject", got.UserSubject, session.UserSubject},
		{"credentialProvider", got.CredentialProvider, session.CredentialProvider},
		{"redirectUri", got.RedirectURI, session.RedirectURI},
	} {
		if field.got != field.want {
			t.Errorf("%s is %q, want %q", field.name, field.got, field.want)
		}
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != "repo:status" {
		t.Errorf("scopes are %v, want [repo:status]", got.Scopes)
	}
	if got.Status != StatusPending {
		t.Errorf("status is %q, want Pending", got.Status)
	}
	if !got.ExpiresAt.Equal(session.ExpiresAt) {
		t.Errorf("expiresAt is %s, want %s", got.ExpiresAt, session.ExpiresAt)
	}
}

// TestASessionIsCreatedPendingWhateverTheCallerAsked covers the one field the
// store overrides. Creating a session already Exchanging would skip the
// single-use check entirely.
func TestASessionIsCreatedPendingWhateverTheCallerAsked(t *testing.T) {
	sessions, _, _ := newStores(t)
	ctx := context.Background()

	session := testSession(time.Now())
	session.Status = StatusCompleted
	if err := sessions.Create(ctx, session); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := sessions.Get(ctx, testSessionKey())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusPending {
		t.Errorf("stored status is %q, want Pending", got.Status)
	}
}

// TestBeginExchangeIsSingleUse is the property the whole store exists for. A
// replayed callback must not reach the provider's token endpoint twice.
func TestBeginExchangeIsSingleUse(t *testing.T) {
	sessions, _, _ := newStores(t)
	ctx := context.Background()
	now := time.Now()

	if err := sessions.Create(ctx, testSession(now)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	claimed, err := sessions.BeginExchange(ctx, testSessionKey(), now)
	if err != nil {
		t.Fatalf("first BeginExchange: %v", err)
	}
	if claimed.Status != StatusExchanging {
		t.Errorf("status is %q, want Exchanging", claimed.Status)
	}
	// The verifier has to come back, since the caller needs it for the
	// exchange and it is the only place it exists.
	if claimed.Verifier == "" {
		t.Error("BeginExchange returned no verifier")
	}

	if _, err := sessions.BeginExchange(ctx, testSessionKey(), now); !errors.Is(err, ErrNotPending) {
		t.Errorf("second BeginExchange returned %v, want ErrNotPending", err)
	}
}

// TestBeginExchangeChecksExpiryBeforeStatus pins the ordering. Answering "the
// state was right but you are too late" confirms a guessed state.
func TestBeginExchangeChecksExpiryBeforeStatus(t *testing.T) {
	sessions, _, _ := newStores(t)
	ctx := context.Background()
	now := time.Now()

	if err := sessions.Create(ctx, testSession(now)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	tests := []struct {
		name    string
		at      time.Time
		claim   bool
		wantErr error
	}{
		{name: "inside the window", at: now, wantErr: nil},
		{name: "after the window", at: now.Add(11 * time.Minute), wantErr: ErrExpired},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A fresh session per case, since claiming is destructive.
			sessions, _, _ := newStores(t)
			if err := sessions.Create(ctx, testSession(now)); err != nil {
				t.Fatalf("Create: %v", err)
			}
			_, err := sessions.BeginExchange(ctx, testSessionKey(), tt.at)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("got %v, want %v", err, tt.wantErr)
			}
		})
	}

	// An already-claimed session that has since expired reports expiry, not
	// ErrNotPending, so the two cannot be told apart by a late caller.
	if _, err := sessions.BeginExchange(ctx, testSessionKey(), now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := sessions.BeginExchange(ctx, testSessionKey(), now.Add(time.Hour)); !errors.Is(err, ErrExpired) {
		t.Errorf("a claimed, expired session returned %v, want ErrExpired", err)
	}
}

// TestCreateRefusesADuplicateState covers a repeated state not silently
// overwriting a session already in flight.
func TestCreateRefusesADuplicateState(t *testing.T) {
	sessions, _, _ := newStores(t)
	ctx := context.Background()

	if err := sessions.Create(ctx, testSession(time.Now())); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := sessions.Create(ctx, testSession(time.Now())); !errors.Is(err, ErrConflict) {
		t.Errorf("second Create returned %v, want ErrConflict", err)
	}
}

// TestRecordsAreSealedAtRest is the §8.2 requirement, checked against the
// stored object rather than against the API.
func TestRecordsAreSealedAtRest(t *testing.T) {
	sessions, delegations, c := newStores(t)
	ctx := context.Background()
	now := time.Now()

	session := testSession(now)
	if err := sessions.Create(ctx, session); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	record := testDelegation(now)
	if err := delegations.Put(ctx, record); err != nil {
		t.Fatalf("Put delegation: %v", err)
	}

	tests := []struct {
		name    string
		secret  string
		secrets []string
	}{
		{
			name:    "session",
			secret:  SessionSecretName(testSessionKey()),
			secrets: []string{session.Verifier, session.UserSubject, session.SandboxUID},
		},
		{
			name:   "delegation",
			secret: DelegationSecretName(testDelegationKey()),
			secrets: []string{
				record.AccessToken, record.RefreshToken, record.ExternalAccountID,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := &corev1.Secret{}
			key := client.ObjectKey{Namespace: storeNamespace, Name: tt.secret}
			if err := c.Get(ctx, key, stored); err != nil {
				t.Fatalf("read the stored Secret: %v", err)
			}

			blob := string(stored.Data[payloadKey])
			if blob == "" {
				t.Fatal("the Secret carries no payload")
			}
			for _, secret := range tt.secrets {
				if strings.Contains(blob, secret) {
					t.Errorf("%q appears in the stored payload", secret)
				}
			}
			// And nothing leaked into a label either.
			for name, value := range stored.Labels {
				for _, secret := range tt.secrets {
					if strings.Contains(value, secret) {
						t.Errorf("%q appears in label %s", secret, name)
					}
				}
			}
		})
	}
}

// TestARecordCannotBeMovedToAnotherAddress is why the key is sealed in as
// associated data. Somebody who can write Secrets must not be able to relocate
// a delegation and have it open.
func TestARecordCannotBeMovedToAnotherAddress(t *testing.T) {
	_, delegations, c := newStores(t)
	ctx := context.Background()

	record := testDelegation(time.Now())
	if err := delegations.Put(ctx, record); err != nil {
		t.Fatalf("Put: %v", err)
	}

	stored := &corev1.Secret{}
	key := client.ObjectKey{Namespace: storeNamespace, Name: DelegationSecretName(testDelegationKey())}
	if err := c.Get(ctx, key, stored); err != nil {
		t.Fatalf("read: %v", err)
	}

	// The attacker copies the sealed payload to the Secret name of another
	// agent's delegation for the same user and provider.
	otherKey := testDelegationKey()
	otherKey.AgentIdentity = "billing-agent"
	moved := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DelegationSecretName(otherKey),
			Namespace: storeNamespace,
		},
		Type: DelegationSecretType,
		Data: map[string][]byte{payloadKey: stored.Data[payloadKey]},
	}
	if err := c.Create(ctx, moved); err != nil {
		t.Fatalf("plant the moved record: %v", err)
	}

	if _, err := delegations.Get(ctx, otherKey); err == nil {
		t.Fatal("a delegation moved to another agent's address opened")
	}
	// And the original still works, so the refusal is about the address.
	if _, err := delegations.Get(ctx, testDelegationKey()); err != nil {
		t.Fatalf("the original stopped opening: %v", err)
	}
}

// TestDelegationPutIsCompareAndSwap covers the §4.4 races: a callback
// completing, a refresh rotating tokens and an invalidation clearing them.
func TestDelegationPutIsCompareAndSwap(t *testing.T) {
	_, delegations, _ := newStores(t)
	ctx := context.Background()
	now := time.Now()

	record := testDelegation(now)
	if err := delegations.Put(ctx, record); err != nil {
		t.Fatalf("create: %v", err)
	}
	if record.Version == "" {
		t.Fatal("create did not record a version")
	}

	// Two readers both see the same version.
	first, err := delegations.Get(ctx, testDelegationKey())
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	second, err := delegations.Get(ctx, testDelegationKey())
	if err != nil {
		t.Fatalf("second read: %v", err)
	}

	// A refresh rotates the tokens and wins.
	first.AccessToken = "gho_rotated"
	if err := delegations.Put(ctx, first); err != nil {
		t.Fatalf("the first writer failed: %v", err)
	}

	// The invalidation raced it and must be told, not silently applied.
	second.State = DelegationNeedsReauthorization
	if err := delegations.Put(ctx, second); !errors.Is(err, ErrConflict) {
		t.Errorf("the second writer returned %v, want ErrConflict", err)
	}

	// The winner's value is what is stored.
	got, err := delegations.Get(ctx, testDelegationKey())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.AccessToken != "gho_rotated" {
		t.Errorf("access token is %q, want the rotated one", got.AccessToken)
	}
	if got.State != DelegationActive {
		t.Errorf("state is %q, want the losing writer not to have applied", got.State)
	}
}

// TestPutWithoutAVersionWillNotClobber covers the caller that forgot to carry
// its version. Treating that as an overwrite would let a stale refresh undo an
// invalidation.
func TestPutWithoutAVersionWillNotClobber(t *testing.T) {
	_, delegations, _ := newStores(t)
	ctx := context.Background()

	if err := delegations.Put(ctx, testDelegation(time.Now())); err != nil {
		t.Fatalf("create: %v", err)
	}

	fresh := testDelegation(time.Now())
	fresh.AccessToken = "gho_fromAStaleWriter"
	if err := delegations.Put(ctx, fresh); !errors.Is(err, ErrConflict) {
		t.Errorf("a versionless Put over an existing record returned %v, want ErrConflict", err)
	}
}

// TestKeysAreDistinctAndCollisionResistant covers the naming. A collision
// means one record overwriting another, which for a delegation means one
// agent inheriting another's authorization.
func TestKeysAreDistinctAndCollisionResistant(t *testing.T) {
	base := testDelegationKey()

	t.Run("every part of the key changes the name", func(t *testing.T) {
		seen := map[string]string{DelegationSecretName(base): "base"}
		for _, tc := range []struct {
			name   string
			mutate func(k *DelegationKey)
		}{
			{"namespace", func(k *DelegationKey) { k.Namespace = "team-b" }},
			{"userIssuer", func(k *DelegationKey) { k.UserIssuer = "https://other.example.com" }},
			{"userSubject", func(k *DelegationKey) { k.UserSubject = "bob@example.com" }},
			{"agentIdentity", func(k *DelegationKey) { k.AgentIdentity = "billing-agent" }},
			{"credentialProvider", func(k *DelegationKey) { k.CredentialProvider = "gitlab" }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				key := base
				tc.mutate(&key)
				name := DelegationSecretName(key)
				if previous, clash := seen[name]; clash {
					t.Fatalf("changing %s collided with %s", tc.name, previous)
				}
				seen[name] = tc.name
			})
		}
	})

	t.Run("a boundary shifted between parts does not collide", func(t *testing.T) {
		// Concatenated these two are identical, which is why the parts are
		// length-prefixed before hashing.
		first := DelegationKey{Namespace: "ab", UserIssuer: "c", UserSubject: "x",
			AgentIdentity: "d", CredentialProvider: "e"}
		second := DelegationKey{Namespace: "a", UserIssuer: "bc", UserSubject: "x",
			AgentIdentity: "d", CredentialProvider: "e"}
		if DelegationSecretName(first) == DelegationSecretName(second) {
			t.Error("two different keys produced the same Secret name")
		}
	})

	t.Run("names are valid Secret names", func(t *testing.T) {
		for _, name := range []string{
			DelegationSecretName(base),
			SessionSecretName(testSessionKey()),
		} {
			if len(name) > 253 {
				t.Errorf("%q is %d characters, over the limit", name, len(name))
			}
			for i := 0; i < len(name); i++ {
				c := name[i]
				ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.'
				if !ok {
					t.Errorf("%q contains %q, which a Secret name may not", name, string(c))
					break
				}
			}
		}
	})
}

// TestMissingRecordsAreNotFound separates absence from failure, since the two
// are different answers to give an agent.
func TestMissingRecordsAreNotFound(t *testing.T) {
	sessions, delegations, _ := newStores(t)
	ctx := context.Background()

	if _, err := sessions.Get(ctx, testSessionKey()); !errors.Is(err, ErrNotFound) {
		t.Errorf("session Get returned %v, want ErrNotFound", err)
	}
	if _, err := delegations.Get(ctx, testDelegationKey()); !errors.Is(err, ErrNotFound) {
		t.Errorf("delegation Get returned %v, want ErrNotFound", err)
	}
	if _, err := sessions.BeginExchange(ctx, testSessionKey(), time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("BeginExchange returned %v, want ErrNotFound", err)
	}
	// Deleting what is not there is not a failure.
	if err := sessions.Delete(ctx, testSessionKey()); err != nil {
		t.Errorf("session Delete on a missing record: %v", err)
	}
	if err := delegations.Delete(ctx, testDelegationKey()); err != nil {
		t.Errorf("delegation Delete on a missing record: %v", err)
	}
}

// TestIncompleteKeysAreRefused covers the addresses that would collapse onto
// one Secret and let one caller read another's record.
func TestIncompleteKeysAreRefused(t *testing.T) {
	sessions, delegations, _ := newStores(t)
	ctx := context.Background()

	t.Run("session", func(t *testing.T) {
		for _, key := range []SessionKey{
			{Namespace: "", StateDigest: "d"},
			{Namespace: "team-a", StateDigest: ""},
		} {
			if _, err := sessions.Get(ctx, key); err == nil {
				t.Errorf("Get accepted %+v", key)
			}
		}
	})

	t.Run("delegation", func(t *testing.T) {
		for _, mutate := range []func(k *DelegationKey){
			func(k *DelegationKey) { k.Namespace = "" },
			func(k *DelegationKey) { k.UserIssuer = "" },
			func(k *DelegationKey) { k.UserSubject = "" },
			func(k *DelegationKey) { k.AgentIdentity = "" },
			func(k *DelegationKey) { k.CredentialProvider = "" },
		} {
			key := testDelegationKey()
			mutate(&key)
			if _, err := delegations.Get(ctx, key); err == nil {
				t.Errorf("Get accepted %+v", key)
			}
		}
	})
}

// TestAStoreWithoutAKeyringIsRefused covers the constructor. A store that
// silently wrote unsealed refresh tokens would be worse than one that will not
// start.
func TestAStoreWithoutAKeyringIsRefused(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	if _, err := NewSecretSessionStore(c, nil, storeNamespace); err == nil {
		t.Error("a session store was built with no keyring")
	}
	if _, err := NewSecretDelegationStore(c, nil, storeNamespace); err == nil {
		t.Error("a delegation store was built with no keyring")
	}
	if _, err := NewSecretDelegationStore(c, testKeyring(t), ""); err == nil {
		t.Error("a store was built with no namespace")
	}
	if _, err := NewSecretDelegationStore(nil, testKeyring(t), storeNamespace); err == nil {
		t.Error("a store was built with no client")
	}
}

// TestFormattingARecordRedactsIt covers §4: a verifier or a token must not
// reach a log, and the likeliest way is a caller printing the struct.
func TestFormattingARecordRedactsIt(t *testing.T) {
	now := time.Now()
	session := testSession(now)
	record := testDelegation(now)

	rendered := fmt.Sprintf("%v %s %+v", *session, *session, *record)
	for _, secret := range []string{
		session.Verifier, record.AccessToken, record.RefreshToken, record.ExternalAccountID,
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("%q appears in formatted output: %s", secret, rendered)
		}
	}
	// The digest identifies the session and is safe, so it must survive.
	if !strings.Contains(rendered, session.Key.StateDigest) {
		t.Error("the state digest was redacted, leaving nothing to identify the session by")
	}
}

// TestFinishEndsTheSession covers the other half of the state machine: a
// finished session is deleted, reports its outcome back to the caller, and
// leaves nothing a replayed callback could claim. Without it a completed
// session would sit in Exchanging until the sweep reached it, one leftover
// Secret per authorization.
func TestFinishEndsTheSession(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	tests := []struct {
		name    string
		status  SessionStatus
		wantErr bool
	}{
		{name: "completed", status: StatusCompleted},
		{name: "failed", status: StatusFailed},
		// Only terminal statuses are accepted. Allowing a move back to Pending
		// would hand the single-use guarantee back to the caller.
		{name: "back to pending is refused", status: StatusPending, wantErr: true},
		{name: "back to exchanging is refused", status: StatusExchanging, wantErr: true},
		{name: "an invented status is refused", status: SessionStatus("Elsewhere"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions, _, _ := newStores(t)
			if err := sessions.Create(ctx, testSession(now)); err != nil {
				t.Fatalf("Create: %v", err)
			}
			claimed, err := sessions.BeginExchange(ctx, testSessionKey(), now)
			if err != nil {
				t.Fatalf("BeginExchange: %v", err)
			}

			err = sessions.Finish(ctx, claimed, tt.status)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Finish accepted %q", tt.status)
				}
				// A refused Finish must remove nothing.
				got, getErr := sessions.Get(ctx, testSessionKey())
				if getErr != nil {
					t.Fatalf("Get after a refused Finish: %v", getErr)
				}
				if got.Status != StatusExchanging {
					t.Errorf("status is %q after a refused Finish, want it left Exchanging", got.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}

			if claimed.Status != tt.status {
				t.Errorf("reported outcome is %q, want %q", claimed.Status, tt.status)
			}
			if claimed.Version != "" {
				t.Errorf("a deleted session still reports version %q", claimed.Version)
			}
			if _, err := sessions.Get(ctx, testSessionKey()); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get on a finished session returned %v, want ErrNotFound", err)
			}
			// A replayed callback resolves nothing and claims nothing.
			if _, err := sessions.ResolveStateDigest(ctx, testSessionKey().StateDigest); !errors.Is(err, ErrNotFound) {
				t.Errorf("resolving a finished session returned %v, want ErrNotFound", err)
			}
			if _, err := sessions.BeginExchange(ctx, testSessionKey(), now); !errors.Is(err, ErrNotFound) {
				t.Errorf("claiming a finished session returned %v, want ErrNotFound", err)
			}
		})
	}
}

// TestFinishRefusesAPendingSession covers the state machine guard directly: a
// session that was never claimed must not be finished. Every case in
// TestFinishEndsTheSession calls BeginExchange first, so without this, a
// caller could skip straight from Pending to Completed and never pass through
// BeginExchange's single-use check at all.
func TestFinishRefusesAPendingSession(t *testing.T) {
	sessions, _, _ := newStores(t)
	ctx := context.Background()
	now := time.Now()

	if err := sessions.Create(ctx, testSession(now)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pending, err := sessions.Get(ctx, testSessionKey())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if pending.Status != StatusPending {
		t.Fatalf("the session is %q, want Pending for this test to mean anything", pending.Status)
	}

	if err := sessions.Finish(ctx, pending, StatusCompleted); !errors.Is(err, ErrNotPending) {
		t.Errorf("Finish on a Pending session returned %v, want ErrNotPending", err)
	}

	got, err := sessions.Get(ctx, testSessionKey())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusPending {
		t.Errorf("status is %q, the refused Finish must not have moved it", got.Status)
	}
}

// TestFinishRequiresAVersion covers the other half of the same gap. Finish is
// meant to take the Session BeginExchange just returned; without this, a
// caller holding only a Key could skip CAS entirely; an empty Version used to
// fall back to an unconditional write.
func TestFinishRequiresAVersion(t *testing.T) {
	sessions, _, _ := newStores(t)
	ctx := context.Background()
	now := time.Now()

	if err := sessions.Create(ctx, testSession(now)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := sessions.BeginExchange(ctx, testSessionKey(), now); err != nil {
		t.Fatalf("BeginExchange: %v", err)
	}

	bare := &Session{Key: testSessionKey()}
	if err := sessions.Finish(ctx, bare, StatusCompleted); err == nil {
		t.Fatal("Finish accepted a Session with no Version")
	}

	got, err := sessions.Get(ctx, testSessionKey())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusExchanging {
		t.Errorf("status is %q, the refused Finish must not have moved it", got.Status)
	}
}

// TestFinishIsCompareAndSwap covers a stale caller finishing a session that
// moved on underneath it.
//
// A sequential stale-copy scenario, call Finish once and reuse the pre-call
// copy, cannot reach this path: by the time the second call's own read
// happens, the status has already moved to terminal and the guard above
// refuses it first. To isolate a genuine version mismatch, an unrelated write
// bumps the stored Secret's resourceVersion without touching status, so the
// session is still genuinely Exchanging when the stale Finish call reads it
// and only the version comparison can tell the caller's copy is behind.
func TestFinishIsCompareAndSwap(t *testing.T) {
	sessions, _, c := newStores(t)
	ctx := context.Background()
	now := time.Now()

	if err := sessions.Create(ctx, testSession(now)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	claimed, err := sessions.BeginExchange(ctx, testSessionKey(), now)
	if err != nil {
		t.Fatalf("BeginExchange: %v", err)
	}
	stale := *claimed

	current := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: storeNamespace, Name: SessionSecretName(testSessionKey())}
	if err := c.Get(ctx, secretKey, current); err != nil {
		t.Fatalf("read the stored secret: %v", err)
	}
	current.Labels["bumped-by-an-unrelated-write"] = "true"
	if err := c.Update(ctx, current); err != nil {
		t.Fatalf("bump the stored secret: %v", err)
	}

	if err := sessions.Finish(ctx, &stale, StatusFailed); !errors.Is(err, ErrConflict) {
		t.Errorf("Finish with a version made stale by an unrelated write returned %v, want ErrConflict", err)
	}

	got, err := sessions.Get(ctx, testSessionKey())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusExchanging {
		t.Errorf("status is %q, the refused Finish must not have moved it", got.Status)
	}
}

// TestFinishDeleteIsCompareAndSwap covers the window the version check in
// Finish cannot: a writer landing after Finish's read but before its delete.
// The delete carries the caller's version as a precondition, so it must lose
// to that writer with ErrConflict and remove nothing, rather than delete a
// session that has moved on underneath it.
func TestFinishDeleteIsCompareAndSwap(t *testing.T) {
	sessions, _, c := newStores(t)
	ctx := context.Background()
	now := time.Now()

	if err := sessions.Create(ctx, testSession(now)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	claimed, err := sessions.BeginExchange(ctx, testSessionKey(), now)
	if err != nil {
		t.Fatalf("BeginExchange: %v", err)
	}

	secretKey := client.ObjectKey{Namespace: storeNamespace, Name: SessionSecretName(testSessionKey())}
	racing := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			// Another writer gets in first, between Finish's read and this
			// delete.
			current := &corev1.Secret{}
			if err := cl.Get(ctx, secretKey, current); err != nil {
				return err
			}
			current.Labels["written-by-a-concurrent-writer"] = "true"
			if err := cl.Update(ctx, current); err != nil {
				return err
			}
			return cl.Delete(ctx, obj, opts...)
		},
	})
	racingStore, err := NewSecretSessionStore(racing, testKeyring(t), storeNamespace)
	if err != nil {
		t.Fatalf("NewSecretSessionStore: %v", err)
	}

	if err := racingStore.Finish(ctx, claimed, StatusCompleted); !errors.Is(err, ErrConflict) {
		t.Fatalf("Finish racing a concurrent writer returned %v, want ErrConflict", err)
	}
	if _, err := sessions.Get(ctx, testSessionKey()); err != nil {
		t.Errorf("the losing Finish deleted the session anyway: %v", err)
	}
}

// TestFinishPrefersConflictToAStaleStatusRead covers the gap zmberg's review
// found: a client backed by an informer cache can still serve the
// pre-exchange Secret to Finish even after BeginExchange's own write landed.
// Checking status first would read that stale copy's Pending and refuse with
// the terminal ErrNotPending, so a session that already exchanged its token
// could never be finished. Checking the version first turns the same read
// into the retryable ErrConflict.
//
// The lag is real here, not stood in for: an interceptor serves the copy of
// the Secret taken before BeginExchange, exactly what a cache that has not
// caught up yet would return, while the caller holds the version
// BeginExchange actually wrote.
func TestFinishPrefersConflictToAStaleStatusRead(t *testing.T) {
	sessions, _, c := newStores(t)
	ctx := context.Background()
	now := time.Now()

	if err := sessions.Create(ctx, testSession(now)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	preExchange := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: storeNamespace, Name: SessionSecretName(testSessionKey())}
	if err := c.Get(ctx, secretKey, preExchange); err != nil {
		t.Fatalf("read the pre-exchange secret: %v", err)
	}
	claimed, err := sessions.BeginExchange(ctx, testSessionKey(), now)
	if err != nil {
		t.Fatalf("BeginExchange: %v", err)
	}
	if claimed.Version == preExchange.ResourceVersion {
		t.Fatal("BeginExchange did not move the version, so this test could not tell a stale read from a current one")
	}

	lagging := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption) error {
			if secret, ok := obj.(*corev1.Secret); ok && key == secretKey {
				preExchange.DeepCopyInto(secret)
				return nil
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	laggingStore, err := NewSecretSessionStore(lagging, testKeyring(t), storeNamespace)
	if err != nil {
		t.Fatalf("NewSecretSessionStore: %v", err)
	}

	if err := laggingStore.Finish(ctx, claimed, StatusCompleted); !errors.Is(err, ErrConflict) {
		t.Fatalf("Finish against a cache still serving the pre-exchange copy returned %v, want ErrConflict", err)
	}
	got, err := sessions.Get(ctx, testSessionKey())
	if err != nil {
		t.Fatalf("the refused Finish removed the session: %v", err)
	}
	if got.Status != StatusExchanging {
		t.Errorf("status is %q, the refused Finish must not have moved it", got.Status)
	}

	// ErrConflict is only worth returning if retrying works: once the cache
	// has caught up, the same caller with the same claimed copy finishes.
	if err := sessions.Finish(ctx, claimed, StatusCompleted); err != nil {
		t.Errorf("Finish once the read is current: %v", err)
	}
}

// TestSessionExpired covers the boundary, which belongs to the expired side: a
// session whose window closes exactly now has no time left.
func TestSessionExpired(t *testing.T) {
	now := time.Now()
	session := &Session{ExpiresAt: now}

	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before the window closes", now.Add(-time.Second), false},
		{"exactly at expiry", now, true},
		{"after", now.Add(time.Second), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := session.Expired(tt.at); got != tt.want {
				t.Errorf("Expired(%s) = %t, want %t", tt.at, got, tt.want)
			}
		})
	}
}

// TestTheIssuerIsNormalisedInTheKey covers the one piece of folding the key
// does, and the limits of it.
//
// Stability is the reason: a delegation is stored against the derived name, so
// the same provider written with and without a trailing slash must not produce
// two records, or a user loses access to an authorization they already granted.
// Everything else is left alone, because folding too much merges two users
// while folding too little only makes one user authorize again.
func TestTheIssuerIsNormalisedInTheKey(t *testing.T) {
	base := testDelegationKey()

	t.Run("a trailing slash on the issuer is folded", func(t *testing.T) {
		slashed := base
		slashed.UserIssuer = base.UserIssuer + "/"
		if DelegationSecretName(slashed) != DelegationSecretName(base) {
			t.Error("the same issuer with a trailing slash produced a different record")
		}
	})

	t.Run("nothing else is folded", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(k *DelegationKey)
		}{
			{"host case", func(k *DelegationKey) { k.UserIssuer = "https://IDP.example.com" }},
			{"scheme case", func(k *DelegationKey) { k.UserIssuer = "HTTPS://idp.example.com" }},
			{"explicit default port", func(k *DelegationKey) { k.UserIssuer = "https://idp.example.com:443" }},
			{"subject case", func(k *DelegationKey) { k.UserSubject = "Alice@example.com" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				key := base
				tc.mutate(&key)
				if DelegationSecretName(key) == DelegationSecretName(base) {
					t.Errorf("%s was folded into the canonical form", tc.name)
				}
			})
		}
	})

	t.Run("the seal is bound to the normalised issuer too", func(t *testing.T) {
		// Otherwise a record written under one spelling would fail to open
		// when looked up by the other, which is the same loss by a different
		// route.
		_, delegations, _ := newStores(t)
		ctx := context.Background()

		record := testDelegation(time.Now())
		if err := delegations.Put(ctx, record); err != nil {
			t.Fatalf("Put: %v", err)
		}
		slashed := base
		slashed.UserIssuer = base.UserIssuer + "/"
		if _, err := delegations.Get(ctx, slashed); err != nil {
			t.Errorf("the record did not open under the slashed spelling: %v", err)
		}
	})
}

// TestSafeLabelValue covers the function on its own: short names pass
// through, long ones become a fixed-length digest that still satisfies a
// label value's own rules.
func TestSafeLabelValue(t *testing.T) {
	t.Run("a name within the limit is unchanged", func(t *testing.T) {
		name := "checkout-agent"
		if got := safeLabelValue(name); got != name {
			t.Errorf("safeLabelValue(%q) = %q, want it unchanged", name, got)
		}
	})

	t.Run("a name at exactly the limit is unchanged", func(t *testing.T) {
		name := strings.Repeat("a", 63)
		if got := safeLabelValue(name); got != name {
			t.Error("a 63-character name was not returned unchanged")
		}
	})

	t.Run("a name over the limit is digested", func(t *testing.T) {
		// A CredentialProvider or AgentIdentity name is a CR metadata.name, a
		// DNS subdomain allowed up to 253 characters, well past what a label
		// value permits. The API server would reject the whole Secret as
		// Invalid rather than merely truncate, so this has to produce
		// something that fits rather than something that doesn't.
		name := strings.Repeat("a", 254)
		got := safeLabelValue(name)
		if len(got) > labelValueLimit {
			t.Errorf("the digest is %d characters, over the %d limit", len(got), labelValueLimit)
		}
		if got == name {
			t.Error("a name over the limit was returned unchanged")
		}
	})

	t.Run("the digest is a valid label value", func(t *testing.T) {
		got := safeLabelValue(strings.Repeat("x", 300))
		if len(got) == 0 {
			t.Fatal("empty digest")
		}
		first, last := got[0], got[len(got)-1]
		isAlnum := func(c byte) bool {
			return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		}
		if !isAlnum(first) || !isAlnum(last) {
			t.Errorf("%q does not start and end with an alphanumeric character", got)
		}
	})

	t.Run("different long names digest differently", func(t *testing.T) {
		a := safeLabelValue(strings.Repeat("a", 300))
		b := safeLabelValue(strings.Repeat("b", 300))
		if a == b {
			t.Error("two different long names produced the same digest")
		}
	})

	t.Run("the same long name always digests the same way", func(t *testing.T) {
		name := strings.Repeat("c", 300)
		if safeLabelValue(name) != safeLabelValue(name) {
			t.Error("the same name produced two different digests")
		}
	})
}

// TestCreateAndPutUseSafeLabelValues covers the wiring: a long AgentIdentity
// or CredentialProvider name must reach the stored Secret's labels through
// safeLabelValue, not as the raw name.
//
// The fake client used here does not enforce the 63-character limit the way a
// real API server would, so this cannot catch a regression by failing to
// create the Secret; it checks the label actually stored instead, which is
// the only way a unit test can tell the wiring is in place without a real
// apiserver.
func TestCreateAndPutUseSafeLabelValues(t *testing.T) {
	longName := strings.Repeat("a", 254)
	ctx := context.Background()

	t.Run("session", func(t *testing.T) {
		sessions, _, c := newStores(t)
		session := testSession(time.Now())
		session.AgentIdentity = longName
		session.CredentialProvider = longName
		if err := sessions.Create(ctx, session); err != nil {
			t.Fatalf("Create: %v", err)
		}

		stored := &corev1.Secret{}
		key := client.ObjectKey{Namespace: storeNamespace, Name: SessionSecretName(session.Key)}
		if err := c.Get(ctx, key, stored); err != nil {
			t.Fatalf("read the stored secret: %v", err)
		}
		if len(stored.Labels[LabelAgentIdentity]) > labelValueLimit {
			t.Errorf("the stored agent identity label is %d characters", len(stored.Labels[LabelAgentIdentity]))
		}
		if len(stored.Labels[LabelCredentialProvider]) > labelValueLimit {
			t.Errorf("the stored credential provider label is %d characters",
				len(stored.Labels[LabelCredentialProvider]))
		}
	})

	t.Run("delegation", func(t *testing.T) {
		_, delegations, c := newStores(t)
		record := testDelegation(time.Now())
		record.Key.AgentIdentity = longName
		record.Key.CredentialProvider = longName
		if err := delegations.Put(ctx, record); err != nil {
			t.Fatalf("Put: %v", err)
		}

		stored := &corev1.Secret{}
		key := client.ObjectKey{Namespace: storeNamespace, Name: DelegationSecretName(record.Key)}
		if err := c.Get(ctx, key, stored); err != nil {
			t.Fatalf("read the stored secret: %v", err)
		}
		if len(stored.Labels[LabelAgentIdentity]) > labelValueLimit {
			t.Errorf("the stored agent identity label is %d characters", len(stored.Labels[LabelAgentIdentity]))
		}
		if len(stored.Labels[LabelCredentialProvider]) > labelValueLimit {
			t.Errorf("the stored credential provider label is %d characters",
				len(stored.Labels[LabelCredentialProvider]))
		}
	})
}

// TestSweepExpiredSessions covers the cleanup path. Nothing else in this
// package reclaims a session that is created and never finished, closing the
// page, declining, or simply walking away, so without a sweep the count of
// oauth-session Secrets would grow with every attempt whether it completes or
// not.
func TestSweepExpiredSessions(t *testing.T) {
	sessions, _, c := newStores(t)
	ctx := context.Background()
	now := time.Now()

	expiredKey := SessionKey{Namespace: "team-a", StateDigest: "expired-one"}
	expired := testSession(now)
	expired.Key = expiredKey
	expired.ExpiresAt = now.Add(-time.Hour)
	if err := sessions.Create(ctx, expired); err != nil {
		t.Fatalf("create the expired session: %v", err)
	}

	liveKey := SessionKey{Namespace: "team-a", StateDigest: "still-live"}
	live := testSession(now)
	live.Key = liveKey
	live.ExpiresAt = now.Add(time.Hour)
	if err := sessions.Create(ctx, live); err != nil {
		t.Fatalf("create the live session: %v", err)
	}

	deleted, err := sessions.SweepExpiredSessions(ctx, now)
	if err != nil {
		t.Fatalf("SweepExpiredSessions: %v", err)
	}
	if deleted != 1 {
		t.Errorf("swept %d sessions, want 1", deleted)
	}

	if _, err := sessions.Get(ctx, expiredKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("the expired session is still there: %v", err)
	}
	if _, err := sessions.Get(ctx, liveKey); err != nil {
		t.Errorf("the live session was swept too: %v", err)
	}

	t.Run("a second sweep finds nothing left to do", func(t *testing.T) {
		deleted, err := sessions.SweepExpiredSessions(ctx, now)
		if err != nil {
			t.Fatalf("SweepExpiredSessions: %v", err)
		}
		if deleted != 0 {
			t.Errorf("swept %d sessions on an already-clean sweep, want 0", deleted)
		}
	})

	t.Run("a secret of a different kind is left alone", func(t *testing.T) {
		// The sweep selects on the record-kind label, not on an annotation
		// alone, so it must not touch an unrelated Secret even if one happens
		// to carry an expiry annotation coincidentally.
		other := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "not-a-session",
				Namespace: storeNamespace,
				Annotations: map[string]string{
					expiresAtAnnotation: now.Add(-time.Hour).UTC().Format(time.RFC3339),
				},
			},
		}
		if err := c.Create(ctx, other); err != nil {
			t.Fatalf("create the unrelated secret: %v", err)
		}
		if _, err := sessions.SweepExpiredSessions(ctx, now); err != nil {
			t.Fatalf("SweepExpiredSessions: %v", err)
		}
		if err := c.Get(ctx, client.ObjectKey{Namespace: storeNamespace, Name: "not-a-session"},
			&corev1.Secret{}); err != nil {
			t.Errorf("the unrelated secret was swept: %v", err)
		}
	})
}

// TestExpiresAtAnnotationRoundTrips covers the plaintext annotation Create
// writes, which is what lets SweepExpiredSessions decide expiry without a
// keyring.
func TestExpiresAtAnnotationRoundTrips(t *testing.T) {
	sessions, _, c := newStores(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	session := testSession(now)
	session.ExpiresAt = now.Add(10 * time.Minute)
	if err := sessions.Create(ctx, session); err != nil {
		t.Fatalf("Create: %v", err)
	}

	stored := &corev1.Secret{}
	key := client.ObjectKey{Namespace: storeNamespace, Name: SessionSecretName(session.Key)}
	if err := c.Get(ctx, key, stored); err != nil {
		t.Fatalf("read the stored secret: %v", err)
	}
	raw, ok := stored.Annotations[expiresAtAnnotation]
	if !ok {
		t.Fatal("the stored secret carries no expiry annotation")
	}
	got, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("the annotation is not RFC3339: %v", err)
	}
	if !got.Equal(session.ExpiresAt) {
		t.Errorf("the annotation reads %s, want %s", got, session.ExpiresAt)
	}
}

// TestResolveStateDigest covers the lookup the OAuth callback actually has
// available: a state digest alone, with no namespace, which is what section
// 7.5 limits a callback request to. Create is what plants the label index
// this depends on.
func TestResolveStateDigest(t *testing.T) {
	sessions, _, _ := newStores(t)
	ctx := context.Background()
	now := time.Now()

	original := testSession(now)
	if err := sessions.Create(ctx, original); err != nil {
		t.Fatalf("Create: %v", err)
	}

	resolved, err := sessions.ResolveStateDigest(ctx, testSessionKey().StateDigest)
	if err != nil {
		t.Fatalf("ResolveStateDigest: %v", err)
	}
	if resolved != testSessionKey() {
		t.Errorf("ResolveStateDigest: got %+v, want %+v", resolved, testSessionKey())
	}

	// The resolved key must actually work, not just look right: it has to
	// address the same session every other method on this store would reach
	// through the full key a caller who already knew the namespace would use.
	got, err := sessions.Get(ctx, resolved)
	if err != nil {
		t.Fatalf("Get with the resolved key: %v", err)
	}
	if got.AgentIdentity != original.AgentIdentity || got.CredentialProvider != original.CredentialProvider {
		t.Error("the session reached through the resolved key is not the one Create stored")
	}
}

// TestResolveStateDigestDoesNotConfuseNamespaces covers the reason this
// exists at all: two different namespaces' sessions must resolve to their own
// namespace, never each other's, even though the callback that calls this
// never names one.
func TestResolveStateDigestDoesNotConfuseNamespaces(t *testing.T) {
	sessions, _, _ := newStores(t)
	ctx := context.Background()
	now := time.Now()

	teamA := testSession(now)
	teamA.Key = SessionKey{Namespace: "team-a", StateDigest: "state-digest-team-a"}
	teamB := testSession(now)
	teamB.Key = SessionKey{Namespace: "team-b", StateDigest: "state-digest-team-b"}

	if err := sessions.Create(ctx, teamA); err != nil {
		t.Fatalf("Create team-a: %v", err)
	}
	if err := sessions.Create(ctx, teamB); err != nil {
		t.Fatalf("Create team-b: %v", err)
	}

	gotA, err := sessions.ResolveStateDigest(ctx, "state-digest-team-a")
	if err != nil {
		t.Fatalf("ResolveStateDigest team-a: %v", err)
	}
	if gotA.Namespace != "team-a" {
		t.Errorf("team-a resolved to namespace %q", gotA.Namespace)
	}

	gotB, err := sessions.ResolveStateDigest(ctx, "state-digest-team-b")
	if err != nil {
		t.Fatalf("ResolveStateDigest team-b: %v", err)
	}
	if gotB.Namespace != "team-b" {
		t.Errorf("team-b resolved to namespace %q", gotB.Namespace)
	}
}

// TestResolveStateDigestRefusesWhatDoesNotExist covers the two ways a lookup
// can fail to find anything: a digest nothing was ever created under, and an
// empty one, which validateSessionKey would also reject if it reached a full
// SessionKey but ResolveStateDigest never constructs one to check.
func TestResolveStateDigestRefusesWhatDoesNotExist(t *testing.T) {
	sessions, _, _ := newStores(t)
	ctx := context.Background()

	if _, err := sessions.ResolveStateDigest(ctx, "a-digest-nothing-was-created-under"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown digest: got %v, want ErrNotFound", err)
	}
	if _, err := sessions.ResolveStateDigest(ctx, ""); err == nil {
		t.Error("an empty digest was accepted")
	}
}

// TestGoStringAndJSONRedactSecrets extends the formatting coverage to the two
// paths String does not reach: %#v, which bypasses the Stringer entirely, and
// json.Marshal, which would write a secret straight into whatever the caller
// is persisting or logging as structured output.
func TestGoStringAndJSONRedactSecrets(t *testing.T) {
	now := time.Now()
	session := testSession(now)
	record := testDelegation(now)

	t.Run("hash-v formatting", func(t *testing.T) {
		rendered := fmt.Sprintf("%#v %#v", *session, *record)
		for _, secret := range []string{
			session.Verifier, record.AccessToken, record.RefreshToken, record.ExternalAccountID,
		} {
			if strings.Contains(rendered, secret) {
				t.Errorf("%q appears under hash-v formatting: %s", secret, rendered)
			}
		}
	})

	t.Run("json.Marshal", func(t *testing.T) {
		sessionJSON, err := json.Marshal(*session)
		if err != nil {
			t.Fatalf("marshal session: %v", err)
		}
		recordJSON, err := json.Marshal(*record)
		if err != nil {
			t.Fatalf("marshal delegation: %v", err)
		}
		rendered := string(sessionJSON) + string(recordJSON)
		for _, secret := range []string{
			session.Verifier, record.AccessToken, record.RefreshToken, record.ExternalAccountID,
		} {
			if strings.Contains(rendered, secret) {
				t.Errorf("%q appears in marshalled JSON: %s", secret, rendered)
			}
		}
		// The secret fields are tagged json colon dash, so they must be
		// absent entirely rather than present with a placeholder.
		if strings.Contains(string(sessionJSON), `"Verifier"`) {
			t.Error("the session's Verifier field key appears in the JSON output")
		}
	})

	t.Run("the safe identifiers still render and serialise", func(t *testing.T) {
		// Redacting these too would make a stored record impossible to find
		// or debug.
		rendered := fmt.Sprintf("%#v", *session)
		if !strings.Contains(rendered, session.Key.StateDigest) {
			t.Error("the state digest was redacted under hash-v formatting")
		}
		encoded, err := json.Marshal(*session)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(encoded), session.UserSubject) {
			t.Error("the user subject did not survive marshalling")
		}
	})
}

// TestResealDelegations covers the half of a key rotation the keyring cannot
// do alone. After a sweep the retired key is dropped, and every delegation
// must still open, which is the only evidence that matters: a sweep that
// re-sealed most records would pass any count-based check and still strand a
// user.
func TestResealDelegations(t *testing.T) {
	previous := aead.Key{ID: "2026-05", Secret: bytes.Repeat([]byte{0x22}, aead.KeySize)}
	current := aead.Key{ID: "2026-11", Secret: bytes.Repeat([]byte{0x11}, aead.KeySize)}
	stranger := aead.Key{ID: "2025-11", Secret: bytes.Repeat([]byte{0x33}, aead.KeySize)}
	keyringOf := func(active aead.Key, retained ...aead.Key) *aead.Keyring {
		t.Helper()
		keyring, err := aead.NewKeyring(active, retained...)
		if err != nil {
			t.Fatalf("build keyring: %v", err)
		}
		return keyring
	}
	keyFor := func(agent string) DelegationKey {
		key := testDelegationKey()
		key.AgentIdentity = agent
		return key
	}
	// Two delegations predate the rotation and one follows it.
	retiredA, retiredB, fresh := keyFor("checkout-agent"), keyFor("billing-agent"), keyFor("support-agent")
	// listedFirst lists the named record ahead of the rest, so a sweep that
	// gave up at a bad record would visibly leave good ones behind.
	listedFirst := func(name string) *interceptor.Funcs {
		return &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
			opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			secrets := list.(*corev1.SecretList).Items
			if i := slices.IndexFunc(secrets, func(s corev1.Secret) bool { return s.Name == name }); i > 0 {
				secrets[0], secrets[i] = secrets[i], secrets[0]
			}
			return nil
		}}
	}
	failUpdateOf := func(name string, err error) *interceptor.Funcs {
		return &interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object,
			opts ...client.UpdateOption) error {
			if obj.GetName() == name {
				return err
			}
			return c.Update(ctx, obj, opts...)
		}}
	}

	tests := []struct {
		name string
		// strangerRecord adds a delegation sealed under a key the rotated
		// keyring never held.
		strangerRecord bool
		// tamper corrupts the ciphertext of retiredA before the sweep.
		tamper    bool
		intercept *interceptor.Funcs
		cancelled bool
		sweeps    int

		wantResealed int
		wantErr      error
		wantErrText  string
		// stranded must still need the retired key afterwards.
		stranded []DelegationKey
	}{
		{
			name:         "every delegation under the retired key moves, and the key can be dropped",
			wantResealed: 2,
		},
		{
			name:         "a second sweep finds nothing left",
			sweeps:       2,
			wantResealed: 0,
		},
		{
			name:           "a delegation under a key nobody holds is reported and the rest still move",
			strangerRecord: true,
			intercept:      listedFirst(DelegationSecretName(keyFor("stranger-agent"))),
			wantResealed:   2,
			wantErr:        aead.ErrUnknownKey,
			wantErrText:    DelegationSecretName(keyFor("stranger-agent")),
		},
		{
			name:         "a delegation that does not open is reported and the rest still move",
			tamper:       true,
			intercept:    listedFirst(DelegationSecretName(retiredA)),
			wantResealed: 1,
			wantErr:      aead.ErrNotAuthentic,
			wantErrText:  DelegationSecretName(retiredA),
			stranded:     []DelegationKey{retiredA},
		},
		{
			// A real write lands between the sweep reading the record and
			// writing it back, so only an actual compare-and-swap leaves the
			// other writer alone.
			name: "a delegation another writer moves mid-sweep is left to that writer",
			intercept: &interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object,
				opts ...client.UpdateOption) error {
				if obj.GetName() == DelegationSecretName(retiredA) && obj.GetAnnotations()["moved"] == "" {
					meanwhile := &corev1.Secret{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), meanwhile); err != nil {
						return err
					}
					meanwhile.Annotations = map[string]string{"moved": "by another writer"}
					if err := c.Update(ctx, meanwhile); err != nil {
						return err
					}
				}
				return c.Update(ctx, obj, opts...)
			}},
			wantResealed: 1,
			stranded:     []DelegationKey{retiredA},
		},
		{
			name: "a delegation withdrawn mid-sweep stays withdrawn",
			intercept: failUpdateOf(DelegationSecretName(retiredA),
				apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, DelegationSecretName(retiredA))),
			wantResealed: 1,
			stranded:     []DelegationKey{retiredA},
		},
		{
			name:         "a failed write is reported and the rest still move",
			intercept:    failUpdateOf(DelegationSecretName(retiredA), errors.New("the API server is unavailable")),
			wantResealed: 1,
			wantErrText:  "write re-sealed " + DelegationSecretName(retiredA),
			stranded:     []DelegationKey{retiredA},
		},
		{
			name: "a failed list re-seals nothing",
			intercept: &interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList,
				...client.ListOption) error {
				return errors.New("the cache is unavailable")
			}},
			wantErrText: "list delegations",
			stranded:    []DelegationKey{retiredA, retiredB},
		},
		{
			name:      "a cancelled sweep stops",
			cancelled: true,
			wantErr:   context.Canceled,
			stranded:  []DelegationKey{retiredA, retiredB},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			builder := fake.NewClientBuilder().WithScheme(testScheme(t))
			if tt.intercept != nil {
				builder = builder.WithInterceptorFuncs(*tt.intercept)
			}
			c := builder.Build()
			storeUnder := func(keyring *aead.Keyring) *SecretDelegationStore {
				t.Helper()
				store, err := NewSecretDelegationStore(c, keyring, storeNamespace)
				if err != nil {
					t.Fatalf("NewSecretDelegationStore: %v", err)
				}
				return store
			}
			put := func(store *SecretDelegationStore, key DelegationKey) {
				t.Helper()
				record := testDelegation(time.Now())
				record.Key = key
				record.AccessToken = "gho_" + key.AgentIdentity
				if err := store.Put(ctx, record); err != nil {
					t.Fatalf("seed %s: %v", key.AgentIdentity, err)
				}
			}
			versionOf := func(name string) string {
				t.Helper()
				secret := &corev1.Secret{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: storeNamespace, Name: name}, secret); err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				return secret.ResourceVersion
			}

			put(storeUnder(keyringOf(previous)), retiredA)
			put(storeUnder(keyringOf(previous)), retiredB)
			rotated := storeUnder(keyringOf(current, previous))
			put(rotated, fresh)
			if tt.strangerRecord {
				put(storeUnder(keyringOf(stranger)), keyFor("stranger-agent"))
			}
			// A session sealed under the retired key, which the sweep must not
			// touch: sessions are waited out, not re-sealed.
			sessions, err := NewSecretSessionStore(c, keyringOf(previous), storeNamespace)
			if err != nil {
				t.Fatalf("NewSecretSessionStore: %v", err)
			}
			if err := sessions.Create(ctx, testSession(time.Now())); err != nil {
				t.Fatalf("seed session: %v", err)
			}
			freshVersion := versionOf(DelegationSecretName(fresh))
			sessionVersion := versionOf(SessionSecretName(testSessionKey()))

			if tt.tamper {
				secret := &corev1.Secret{}
				name := client.ObjectKey{Namespace: storeNamespace, Name: DelegationSecretName(retiredA)}
				if err := c.Get(ctx, name, secret); err != nil {
					t.Fatalf("read: %v", err)
				}
				var envelope map[string]any
				if err := json.Unmarshal(secret.Data[payloadKey], &envelope); err != nil {
					t.Fatalf("decode envelope: %v", err)
				}
				envelope["ct"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
				if secret.Data[payloadKey], err = json.Marshal(envelope); err != nil {
					t.Fatalf("encode envelope: %v", err)
				}
				if err := c.Update(ctx, secret); err != nil {
					t.Fatalf("tamper: %v", err)
				}
			}

			sweepCtx := ctx
			if tt.cancelled {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				sweepCtx = cancelled
			}
			var resealed int
			for range max(tt.sweeps, 1) {
				resealed, err = rotated.ResealDelegations(sweepCtx)
			}

			if resealed != tt.wantResealed {
				t.Errorf("re-sealed: got %d, want %d", resealed, tt.wantResealed)
			}
			if tt.wantErr == nil && tt.wantErrText == "" {
				if err != nil {
					t.Fatalf("ResealDelegations: %v", err)
				}
			} else {
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Errorf("error: got %v, want %v", err, tt.wantErr)
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Errorf("error: got %v, want one containing %q", err, tt.wantErrText)
				}
			}

			// Drop the retired key. Everything not stranded opens, still
			// holding what was sealed into it.
			afterDrop := storeUnder(keyringOf(current))
			for _, key := range []DelegationKey{retiredA, retiredB, fresh} {
				got, err := afterDrop.Get(ctx, key)
				if slices.Contains(tt.stranded, key) {
					if err == nil {
						t.Errorf("%s opened without the retired key, want it still needing it", key.AgentIdentity)
					}
					continue
				}
				if err != nil {
					t.Errorf("%s does not open once the retired key is dropped: %v", key.AgentIdentity, err)
					continue
				}
				if want := "gho_" + key.AgentIdentity; got.AccessToken != want {
					t.Errorf("%s access token: got %q, want %q", key.AgentIdentity, got.AccessToken, want)
				}
			}
			if got := versionOf(DelegationSecretName(fresh)); got != freshVersion {
				t.Error("a delegation already under the active key was rewritten")
			}
			if got := versionOf(SessionSecretName(testSessionKey())); got != sessionVersion {
				t.Error("the sweep rewrote a session")
			}
		})
	}
}
