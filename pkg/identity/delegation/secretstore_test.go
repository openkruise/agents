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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

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

// TestFinishRecordsATerminalStatus covers the other half of the state machine.
// Without it a completed session stays Exchanging forever and nothing can tell
// a finished round trip from one still in flight.
func TestFinishRecordsATerminalStatus(t *testing.T) {
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
				return
			}
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}

			got, err := sessions.Get(ctx, testSessionKey())
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != tt.status {
				t.Errorf("stored status is %q, want %q", got.Status, tt.status)
			}
			// A finished session cannot be claimed again.
			if _, err := sessions.BeginExchange(ctx, testSessionKey(), now); !errors.Is(err, ErrNotPending) {
				t.Errorf("claiming a finished session returned %v, want ErrNotPending", err)
			}
		})
	}
}

// TestFinishIsCompareAndSwap covers a stale caller finishing a session that
// moved on underneath it.
func TestFinishIsCompareAndSwap(t *testing.T) {
	sessions, _, _ := newStores(t)
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
	if err := sessions.Finish(ctx, claimed, StatusCompleted); err != nil {
		t.Fatalf("the first Finish failed: %v", err)
	}
	if err := sessions.Finish(ctx, &stale, StatusFailed); !errors.Is(err, ErrConflict) {
		t.Errorf("the stale Finish returned %v, want ErrConflict", err)
	}

	got, err := sessions.Get(ctx, testSessionKey())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status is %q, want the first writer to have won", got.Status)
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
