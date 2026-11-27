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

package credential

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/pkg/identity/aead"
	"github.com/openkruise/agents/pkg/identity/delegation"
)

const (
	storeNamespace = "sandbox-system"
	oldAccess      = "gho_old_access_token"
	oldRefresh     = "ghr_old_refresh_token"
	newAccess      = "ghu_new_access_token"
	newRefresh     = "ghr_new_refresh_token"
	winnerAccess   = "ghu_winning_replica_access_token"
)

var testKey = delegation.DelegationKey{
	Namespace:          testNamespace,
	UserIssuer:         "https://idp.example.com/realms/agents",
	UserSubject:        "alice",
	AgentIdentity:      "checkout-agent",
	CredentialProvider: testProvider,
}

// respondFunc answers one token endpoint request. It is handed the fixture so
// a case can change the store mid-request, the way another replica would.
type respondFunc func(f *refreshFixture, form url.Values) (int, map[string]any)

type fixtureConfig struct {
	// respond answers the token endpoint. Nil means a refresh is not expected,
	// and a request fails the test.
	respond respondFunc
	// objects builds the cluster around the token endpoint URL. Nil means a
	// Ready provider and its client secret.
	objects func(tokenURL string) []client.Object
	// intercept, when set, wraps the client the Refresher and its store read
	// and write through. Nothing else goes through it, so it sees only the
	// Refresher's own traffic.
	intercept func(f *refreshFixture) interceptor.Funcs
}

// refreshFixture is a Refresher over a real SecretDelegationStore on a fake
// cluster, talking to a real token endpoint.
type refreshFixture struct {
	refresher *Refresher
	// other is a second store over the same records, standing in for every
	// writer that is not this Refresher: another replica, a callback, an
	// invalidation.
	other *delegation.SecretDelegationStore
	calls atomic.Int32

	mu    sync.Mutex
	forms []url.Values
}

func newRefreshFixture(t *testing.T, cfg fixtureConfig) *refreshFixture {
	t.Helper()
	f := &refreshFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.forms = append(f.forms, r.PostForm)
		f.mu.Unlock()
		if cfg.respond == nil {
			t.Errorf("the token endpoint was called, want no refresh")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		status, body := cfg.respond(f, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)

	objects := []client.Object{readyProvider(server.URL + "/login/oauth/access_token"), usableSecret()}
	if cfg.objects != nil {
		objects = cfg.objects(server.URL + "/login/oauth/access_token")
	}
	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	secret := make([]byte, aead.KeySize)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("generate keyring secret: %v", err)
	}
	keyring, err := aead.NewKeyring(aead.Key{ID: "test-1", Secret: secret})
	if err != nil {
		t.Fatalf("build keyring: %v", err)
	}
	if f.other, err = delegation.NewSecretDelegationStore(direct, keyring, storeNamespace); err != nil {
		t.Fatalf("build delegation store: %v", err)
	}

	var refresherClient client.Client = direct
	if cfg.intercept != nil {
		refresherClient = interceptor.NewClient(direct.(client.WithWatch), cfg.intercept(f))
	}
	store, err := delegation.NewSecretDelegationStore(refresherClient, keyring, storeNamespace)
	if err != nil {
		t.Fatalf("build delegation store: %v", err)
	}
	f.refresher = NewRefresher(refresherClient, store)
	// Long enough for a write a case schedules to land inside it, short
	// enough that every plain refusal does not cost the default.
	f.refresher.refusalGrace = 300 * time.Millisecond
	return f
}

// seed stores record as a new delegation and returns it as stored.
func (f *refreshFixture) seed(t *testing.T, record *delegation.Delegation) *delegation.Delegation {
	t.Helper()
	seeded := *record
	if err := f.other.Put(context.Background(), &seeded); err != nil {
		t.Fatalf("seed delegation: %v", err)
	}
	return &seeded
}

// stored reads the delegation back, or returns nil if there is none.
func (f *refreshFixture) stored(t *testing.T) *delegation.Delegation {
	t.Helper()
	record, err := f.other.Get(context.Background(), testKey)
	if errors.Is(err, delegation.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatalf("read delegation: %v", err)
	}
	return record
}

// rewrite changes the stored delegation the way another writer would.
func (f *refreshFixture) rewrite(t *testing.T, change func(*delegation.Delegation)) {
	t.Helper()
	record := f.stored(t)
	change(record)
	if err := f.other.Put(context.Background(), record); err != nil {
		t.Errorf("rewrite delegation as another writer: %v", err)
	}
}

// markedByARefusedReplica changes a delegation the way requireReauthorization
// does on another replica whose refresh was refused.
func markedByARefusedReplica(d *delegation.Delegation) {
	d.State = delegation.DelegationNeedsReauthorization
	d.AccessToken, d.RefreshToken = "", ""
	d.AccessTokenExpiresAt = time.Time{}
}

func activeDelegation(expiresIn time.Duration, refreshToken string) *delegation.Delegation {
	return &delegation.Delegation{
		Key:                  testKey,
		AccessToken:          oldAccess,
		RefreshToken:         refreshToken,
		ExternalAccountID:    "583231",
		Scopes:               []string{"gist", "repo"},
		AccessTokenExpiresAt: time.Now().Add(expiresIn),
		State:                delegation.DelegationActive,
	}
}

// refreshedWith answers with a new token pair, as GitHub does, adjusted by
// changes; a nil value removes that field.
func refreshedWith(changes map[string]any) respondFunc {
	return func(*refreshFixture, url.Values) (int, map[string]any) {
		body := map[string]any{
			"access_token":  newAccess,
			"refresh_token": newRefresh,
			"token_type":    "bearer",
			"expires_in":    28800,
		}
		for field, value := range changes {
			if value == nil {
				delete(body, field)
				continue
			}
			body[field] = value
		}
		return http.StatusOK, body
	}
}

func rejectedWith(status int, code string) respondFunc {
	return func(*refreshFixture, url.Values) (int, map[string]any) {
		return status, map[string]any{"error": code, "error_description": "refused: " + oldRefresh}
	}
}

func TestToken(t *testing.T) {
	inEightHours := time.Now().Add(8 * time.Hour)
	notReady := func(tokenURL string) []client.Object {
		provider := readyProvider(tokenURL)
		provider.Status.Conditions[0].Status = metav1.ConditionFalse
		return []client.Object{provider, usableSecret()}
	}
	noProvider := func(string) []client.Object { return []client.Object{usableSecret()} }

	tests := []struct {
		name    string
		seed    *delegation.Delegation
		objects func(string) []client.Object
		respond respondFunc

		wantErr       error
		wantErrText   string
		wantCalls     int32
		wantReturned  string
		wantUntouched bool
		// wantStored is compared on state, both tokens, scopes and, to the
		// minute, expiry.
		wantStored *delegation.Delegation
	}{
		{
			name:    "a user who never authorized",
			wantErr: delegation.ErrNotFound,
		},
		{
			name: "a delegation already marked for reauthorization",
			seed: &delegation.Delegation{Key: testKey, Scopes: []string{"repo"},
				State: delegation.DelegationNeedsReauthorization},
			wantErr:       ErrReauthorizationRequired,
			wantUntouched: true,
		},
		{
			name:          "a token good beyond the margin is returned as stored",
			seed:          activeDelegation(time.Hour, oldRefresh),
			wantReturned:  oldAccess,
			wantUntouched: true,
		},
		{
			name: "a token with no expiry never needs refreshing",
			seed: func() *delegation.Delegation {
				d := activeDelegation(0, "")
				d.AccessTokenExpiresAt = time.Time{}
				return d
			}(),
			wantReturned:  oldAccess,
			wantUntouched: true,
		},
		{
			name:         "a token inside the margin is refreshed and stored",
			seed:         activeDelegation(time.Minute, oldRefresh),
			respond:      refreshedWith(nil),
			wantCalls:    1,
			wantReturned: newAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: newAccess,
				RefreshToken: newRefresh, Scopes: []string{"gist", "repo"}, AccessTokenExpiresAt: inEightHours},
		},
		{
			name:         "an already expired token is refreshed",
			seed:         activeDelegation(-time.Hour, oldRefresh),
			respond:      refreshedWith(nil),
			wantCalls:    1,
			wantReturned: newAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: newAccess,
				RefreshToken: newRefresh, Scopes: []string{"gist", "repo"}, AccessTokenExpiresAt: inEightHours},
		},
		{
			name: "a missing access token is refreshed",
			seed: func() *delegation.Delegation {
				d := activeDelegation(time.Hour, oldRefresh)
				d.AccessToken = ""
				return d
			}(),
			respond:      refreshedWith(nil),
			wantCalls:    1,
			wantReturned: newAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: newAccess,
				RefreshToken: newRefresh, Scopes: []string{"gist", "repo"}, AccessTokenExpiresAt: inEightHours},
		},
		{
			name:         "a response without a refresh token keeps the old one",
			seed:         activeDelegation(time.Minute, oldRefresh),
			respond:      refreshedWith(map[string]any{"refresh_token": nil}),
			wantCalls:    1,
			wantReturned: newAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: newAccess,
				RefreshToken: oldRefresh, Scopes: []string{"gist", "repo"}, AccessTokenExpiresAt: inEightHours},
		},
		{
			name:         "a response without expires_in stores a token that does not expire",
			seed:         activeDelegation(time.Minute, oldRefresh),
			respond:      refreshedWith(map[string]any{"expires_in": nil}),
			wantCalls:    1,
			wantReturned: newAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: newAccess,
				RefreshToken: newRefresh, Scopes: []string{"gist", "repo"}},
		},
		{
			name:         "a narrower comma-separated scope replaces the stored one",
			seed:         activeDelegation(time.Minute, oldRefresh),
			respond:      refreshedWith(map[string]any{"scope": "repo"}),
			wantCalls:    1,
			wantReturned: newAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: newAccess,
				RefreshToken: newRefresh, Scopes: []string{"repo"}, AccessTokenExpiresAt: inEightHours},
		},
		{
			name:         "an empty scope, as GitHub Apps send, leaves the grant unchanged",
			seed:         activeDelegation(time.Minute, oldRefresh),
			respond:      refreshedWith(map[string]any{"scope": ""}),
			wantCalls:    1,
			wantReturned: newAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: newAccess,
				RefreshToken: newRefresh, Scopes: []string{"gist", "repo"}, AccessTokenExpiresAt: inEightHours},
		},
		{
			name:    "no refresh token near expiry needs the user again",
			seed:    activeDelegation(time.Minute, ""),
			wantErr: ErrReauthorizationRequired,
			wantStored: &delegation.Delegation{State: delegation.DelegationNeedsReauthorization,
				Scopes: []string{"gist", "repo"}},
		},
		{
			name:      "invalid_grant marks the delegation and clears its tokens",
			seed:      activeDelegation(time.Minute, oldRefresh),
			respond:   rejectedWith(http.StatusBadRequest, "invalid_grant"),
			wantCalls: 1,
			wantErr:   ErrReauthorizationRequired,
			wantStored: &delegation.Delegation{State: delegation.DelegationNeedsReauthorization,
				Scopes: []string{"gist", "repo"}},
		},
		{
			name:      "GitHub's bad_refresh_token, sent with a 200, is the same refusal",
			seed:      activeDelegation(time.Minute, oldRefresh),
			respond:   rejectedWith(http.StatusOK, "bad_refresh_token"),
			wantCalls: 1,
			wantErr:   ErrReauthorizationRequired,
			wantStored: &delegation.Delegation{State: delegation.DelegationNeedsReauthorization,
				Scopes: []string{"gist", "repo"}},
		},
		{
			name:          "invalid_client is the operator's problem and keeps the delegation",
			seed:          activeDelegation(time.Minute, oldRefresh),
			respond:       rejectedWith(http.StatusUnauthorized, "invalid_client"),
			wantCalls:     1,
			wantErrText:   `answered 401 Unauthorized with error "invalid_client"`,
			wantUntouched: true,
		},
		{
			name:          "a provider outage keeps the delegation",
			seed:          activeDelegation(time.Minute, oldRefresh),
			respond:       rejectedWith(http.StatusServiceUnavailable, "temporarily_unavailable"),
			wantCalls:     1,
			wantErrText:   "503 Service Unavailable",
			wantUntouched: true,
		},
		{
			name: "a success response with no access token is refused",
			seed: activeDelegation(time.Minute, oldRefresh),
			respond: func(*refreshFixture, url.Values) (int, map[string]any) {
				return http.StatusOK, map[string]any{"token_type": "bearer"}
			},
			wantCalls:     1,
			wantErrText:   "missing access_token",
			wantUntouched: true,
		},
		{
			name:          "a provider that is not ready is not called",
			seed:          activeDelegation(time.Minute, oldRefresh),
			objects:       notReady,
			wantErr:       ErrProviderNotReady,
			wantUntouched: true,
		},
		{
			name:          "a provider that is gone is not called",
			seed:          activeDelegation(time.Minute, oldRefresh),
			objects:       noProvider,
			wantErr:       ErrProviderNotFound,
			wantUntouched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRefreshFixture(t, fixtureConfig{respond: tt.respond, objects: tt.objects})
			var seeded *delegation.Delegation
			if tt.seed != nil {
				seeded = f.seed(t, tt.seed)
			}

			got, err := f.refresher.Token(context.Background(), testKey)

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error: got %v, want %v", err, tt.wantErr)
				}
			case tt.wantErrText != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("error: got %v, want one containing %q", err, tt.wantErrText)
				}
			case err != nil:
				t.Fatalf("Token: %v", err)
			case got.AccessToken != tt.wantReturned:
				t.Errorf("returned access token: got %q, want %q", got.AccessToken, tt.wantReturned)
			}
			if err != nil {
				for _, secret := range []string{oldRefresh, newRefresh, testClientSecret} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error %q carries a secret", err)
					}
				}
			}

			if calls := f.calls.Load(); calls != tt.wantCalls {
				t.Errorf("token endpoint calls: got %d, want %d", calls, tt.wantCalls)
			}
			if tt.wantCalls > 0 {
				form := f.forms[0]
				for field, want := range map[string]string{
					"grant_type":    "refresh_token",
					"refresh_token": oldRefresh,
					"client_id":     testClientID,
					"client_secret": testClientSecret,
				} {
					if got := form.Get(field); got != want {
						t.Errorf("request %s: got %q, want %q", field, got, want)
					}
				}
			}

			stored := f.stored(t)
			if tt.wantUntouched && seeded != nil {
				if stored == nil || stored.Version != seeded.Version {
					t.Errorf("the delegation was rewritten, want it left as stored")
				}
			}
			if tt.wantStored != nil {
				assertStored(t, stored, tt.wantStored)
			}
		})
	}
}

func assertStored(t *testing.T, got, want *delegation.Delegation) {
	t.Helper()
	if got == nil {
		t.Fatal("no delegation is stored")
	}
	if got.State != want.State {
		t.Errorf("stored state: got %q, want %q", got.State, want.State)
	}
	if got.AccessToken != want.AccessToken {
		t.Errorf("stored access token: got %q, want %q", got.AccessToken, want.AccessToken)
	}
	if got.RefreshToken != want.RefreshToken {
		t.Errorf("stored refresh token: got %q, want %q", got.RefreshToken, want.RefreshToken)
	}
	if !slices.Equal(got.Scopes, want.Scopes) {
		t.Errorf("stored scopes: got %v, want %v", got.Scopes, want.Scopes)
	}
	if got.AccessTokenExpiresAt.IsZero() != want.AccessTokenExpiresAt.IsZero() ||
		got.AccessTokenExpiresAt.Sub(want.AccessTokenExpiresAt).Abs() > time.Minute {
		t.Errorf("stored expiry: got %v, want about %v", got.AccessTokenExpiresAt, want.AccessTokenExpiresAt)
	}
}

// TestTokenSettlesLostRaces covers section 8.2's races among refreshes,
// callbacks and invalidations. Each case changes the store while the token
// endpoint request is in flight, which is what another replica, a callback or
// an invalidation looks like from inside one refresh.
//
// The first two are why compare-and-swap alone is not the whole answer.
// GitHub spends a refresh token on first use, so of two replicas refreshing
// together one succeeds and the other is told the token is bad. Whichever of
// them writes second has to recognise that, or the loser's refusal costs the
// user an authorization the winner had just renewed.
func TestTokenSettlesLostRaces(t *testing.T) {
	tests := []struct {
		name    string
		respond respondFunc

		wantErr      error
		wantReturned string
		// wantStored nil means the delegation must be gone.
		wantStored *delegation.Delegation
	}{
		{
			name: "the losing replica marked it first, and the winner writes over the mark",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				f.rewrite(t, markedByARefusedReplica)
				return refreshedWith(nil)(f, form)
			},
			wantReturned: newAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: newAccess,
				RefreshToken: newRefresh, Scopes: []string{"gist", "repo"},
				AccessTokenExpiresAt: time.Now().Add(8 * time.Hour)},
		},
		{
			name: "the winning replica stored first, and the loser's refusal is not marked",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				f.rewrite(t, func(d *delegation.Delegation) {
					d.AccessToken, d.RefreshToken = winnerAccess, "ghr_winner_refresh_token"
					d.AccessTokenExpiresAt = time.Now().Add(8 * time.Hour)
				})
				return rejectedWith(http.StatusOK, "bad_refresh_token")(f, form)
			},
			wantReturned: winnerAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: winnerAccess,
				RefreshToken: "ghr_winner_refresh_token", Scopes: []string{"gist", "repo"},
				AccessTokenExpiresAt: time.Now().Add(8 * time.Hour)},
		},
		{
			name: "a refusal is held until the winning replica's write lands",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				// The winner's write arrives just after this refusal does,
				// which is the order a real race produces.
				go func() {
					time.Sleep(30 * time.Millisecond)
					f.rewrite(t, func(d *delegation.Delegation) {
						d.AccessToken, d.RefreshToken = winnerAccess, "ghr_winner_refresh_token"
						d.AccessTokenExpiresAt = time.Now().Add(8 * time.Hour)
					})
				}()
				return rejectedWith(http.StatusOK, "bad_refresh_token")(f, form)
			},
			wantReturned: winnerAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: winnerAccess,
				RefreshToken: "ghr_winner_refresh_token", Scopes: []string{"gist", "repo"},
				AccessTokenExpiresAt: time.Now().Add(8 * time.Hour)},
		},
		{
			name: "a fresher record written mid-refresh wins over this refresh",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				f.rewrite(t, func(d *delegation.Delegation) {
					d.AccessToken = winnerAccess
					d.AccessTokenExpiresAt = time.Now().Add(8 * time.Hour)
				})
				return refreshedWith(nil)(f, form)
			},
			wantReturned: winnerAccess,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive, AccessToken: winnerAccess,
				RefreshToken: oldRefresh, Scopes: []string{"gist", "repo"},
				AccessTokenExpiresAt: time.Now().Add(8 * time.Hour)},
		},
		{
			name: "a delegation withdrawn mid-refresh is not brought back",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				if err := f.other.Delete(context.Background(), testKey); err != nil {
					t.Errorf("withdraw delegation: %v", err)
				}
				return refreshedWith(nil)(f, form)
			},
			wantErr: delegation.ErrNotFound,
		},
		{
			name: "a delegation withdrawn while being marked stays withdrawn",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				if err := f.other.Delete(context.Background(), testKey); err != nil {
					t.Errorf("withdraw delegation: %v", err)
				}
				return rejectedWith(http.StatusBadRequest, "invalid_grant")(f, form)
			},
			wantErr: delegation.ErrNotFound,
		},
		{
			name: "two replicas both refused leave it marked once",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				f.rewrite(t, markedByARefusedReplica)
				return rejectedWith(http.StatusBadRequest, "invalid_grant")(f, form)
			},
			wantErr: ErrReauthorizationRequired,
			wantStored: &delegation.Delegation{State: delegation.DelegationNeedsReauthorization,
				Scopes: []string{"gist", "repo"}},
		},
		{
			name: "a still-stale record written while marking is a conflict to retry",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				f.rewrite(t, func(d *delegation.Delegation) { d.AccessToken = "gho_other_but_expiring" })
				return rejectedWith(http.StatusBadRequest, "invalid_grant")(f, form)
			},
			wantErr: delegation.ErrConflict,
			wantStored: &delegation.Delegation{State: delegation.DelegationActive,
				AccessToken: "gho_other_but_expiring", RefreshToken: oldRefresh, Scopes: []string{"gist", "repo"},
				AccessTokenExpiresAt: time.Now().Add(time.Minute)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRefreshFixture(t, fixtureConfig{respond: tt.respond})
			f.seed(t, activeDelegation(time.Minute, oldRefresh))

			got, err := f.refresher.Token(context.Background(), testKey)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error: got %v, want %v", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("Token: %v", err)
				}
				if got.AccessToken != tt.wantReturned {
					t.Errorf("returned access token: got %q, want %q", got.AccessToken, tt.wantReturned)
				}
			}

			stored := f.stored(t)
			if tt.wantStored == nil {
				if stored != nil {
					t.Errorf("the delegation is stored again as %v, want it to stay withdrawn", stored)
				}
				return
			}
			assertStored(t, stored, tt.wantStored)
		})
	}
}

// TestTokenRereadsBeforeEachDecision covers the reads Token makes after its
// first: inside the flight, since the record may have moved since this
// caller looked, and after losing a write, since what won decides what is
// returned. Each case acts on the Refresher's nth read or write of the
// delegation, which is the only way to land a change in those windows
// deterministically.
func TestTokenRereadsBeforeEachDecision(t *testing.T) {
	storeFailure := errors.New("the store is unavailable")
	fail := func(*refreshFixture) error { return storeFailure }
	conflict := func(*refreshFixture) error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "delegation", errors.New("moved again"))
	}

	tests := []struct {
		name    string
		respond respondFunc
		// beforeRead and beforeWrite run before the Refresher's nth read or
		// write of the delegation is served, counting from 1. An error they
		// return fails that operation.
		beforeRead  map[int]func(*refreshFixture) error
		beforeWrite map[int]func(*refreshFixture) error
		// staleReads are served the delegation as the first read saw it, the
		// way an informer cache that has not caught up would serve them.
		staleReads []int
		// seed overrides the delegation every other case starts from.
		seed *delegation.Delegation

		wantErr      error
		wantReturned string
		wantCalls    int32
	}{
		{
			name: "another replica refreshed it between this read and the flight",
			beforeRead: map[int]func(*refreshFixture) error{2: func(f *refreshFixture) error {
				f.rewrite(t, func(d *delegation.Delegation) {
					d.AccessToken = winnerAccess
					d.AccessTokenExpiresAt = time.Now().Add(8 * time.Hour)
				})
				return nil
			}},
			wantReturned: winnerAccess,
		},
		{
			name: "another replica marked it between this read and the flight",
			beforeRead: map[int]func(*refreshFixture) error{2: func(f *refreshFixture) error {
				f.rewrite(t, markedByARefusedReplica)
				return nil
			}},
			wantErr: ErrReauthorizationRequired,
		},
		{
			name:       "a failed read inside the flight is surfaced",
			beforeRead: map[int]func(*refreshFixture) error{2: fail},
			wantErr:    storeFailure,
		},
		{
			name: "a failed read after losing the write is surfaced",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				f.rewrite(t, func(d *delegation.Delegation) { d.UpdatedAt = time.Now() })
				return refreshedWith(nil)(f, form)
			},
			beforeRead: map[int]func(*refreshFixture) error{3: fail},
			wantErr:    storeFailure,
			wantCalls:  1,
		},
		{
			name:        "a failed mark is surfaced",
			seed:        activeDelegation(time.Minute, ""),
			beforeWrite: map[int]func(*refreshFixture) error{1: fail},
			wantErr:     storeFailure,
		},
		{
			name: "a failed read after losing the mark is surfaced",
			seed: activeDelegation(time.Minute, ""),
			beforeWrite: map[int]func(*refreshFixture) error{1: func(f *refreshFixture) error {
				f.rewrite(t, func(d *delegation.Delegation) { d.UpdatedAt = time.Now() })
				return nil
			}},
			beforeRead: map[int]func(*refreshFixture) error{3: fail},
			wantErr:    storeFailure,
		},
		{
			name: "a failed read while a refusal is held is surfaced",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				f.rewrite(t, func(d *delegation.Delegation) { d.UpdatedAt = time.Now() })
				return rejectedWith(http.StatusBadRequest, "invalid_grant")(f, form)
			},
			beforeRead: map[int]func(*refreshFixture) error{3: fail},
			wantErr:    storeFailure,
			wantCalls:  1,
		},
		{
			name: "a cache still showing the rejected version is waited out before writing over a mark",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				f.rewrite(t, markedByARefusedReplica)
				return refreshedWith(nil)(f, form)
			},
			staleReads:   []int{3, 4},
			wantReturned: newAccess,
			wantCalls:    1,
		},
		{
			name: "a cache still showing the rejected version is waited out before trusting a refusal",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				f.rewrite(t, func(d *delegation.Delegation) {
					d.AccessToken, d.RefreshToken = winnerAccess, "ghr_winner_refresh_token"
					d.AccessTokenExpiresAt = time.Now().Add(8 * time.Hour)
				})
				return rejectedWith(http.StatusOK, "bad_refresh_token")(f, form)
			},
			staleReads:   []int{3, 4},
			wantReturned: winnerAccess,
			wantCalls:    1,
		},
		{
			name:    "a winning write landing after the grace still beats the mark",
			respond: rejectedWith(http.StatusBadRequest, "invalid_grant"),
			beforeWrite: map[int]func(*refreshFixture) error{1: func(f *refreshFixture) error {
				f.rewrite(t, func(d *delegation.Delegation) {
					d.AccessToken = winnerAccess
					d.AccessTokenExpiresAt = time.Now().Add(8 * time.Hour)
				})
				return nil
			}},
			wantReturned: winnerAccess,
			wantCalls:    1,
		},
		{
			name: "a record that moves again after the mark is a conflict to retry",
			respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
				f.rewrite(t, markedByARefusedReplica)
				return refreshedWith(nil)(f, form)
			},
			// The first write conflicts on its own; the retry is made to as well.
			beforeWrite: map[int]func(*refreshFixture) error{2: conflict},
			wantErr:     delegation.ErrConflict,
			wantCalls:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reads, writes atomic.Int32
			var firstRead *corev1.Secret
			f := newRefreshFixture(t, fixtureConfig{
				respond: tt.respond,
				intercept: func(f *refreshFixture) interceptor.Funcs {
					return interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
							obj client.Object, opts ...client.GetOption) error {
							if key.Namespace != storeNamespace {
								return c.Get(ctx, key, obj, opts...)
							}
							n := int(reads.Add(1))
							if hook := tt.beforeRead[n]; hook != nil {
								if err := hook(f); err != nil {
									return err
								}
							}
							if slices.Contains(tt.staleReads, n) {
								*obj.(*corev1.Secret) = *firstRead.DeepCopy()
								return nil
							}
							if err := c.Get(ctx, key, obj, opts...); err != nil {
								return err
							}
							if n == 1 {
								firstRead = obj.(*corev1.Secret).DeepCopy()
							}
							return nil
						},
						Update: func(ctx context.Context, c client.WithWatch, obj client.Object,
							opts ...client.UpdateOption) error {
							if obj.GetNamespace() == storeNamespace {
								if hook := tt.beforeWrite[int(writes.Add(1))]; hook != nil {
									if err := hook(f); err != nil {
										return err
									}
								}
							}
							return c.Update(ctx, obj, opts...)
						},
					}
				},
			})
			seed := tt.seed
			if seed == nil {
				seed = activeDelegation(time.Minute, oldRefresh)
			}
			f.seed(t, seed)

			got, err := f.refresher.Token(context.Background(), testKey)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error: got %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Token: %v", err)
			} else if got.AccessToken != tt.wantReturned {
				t.Errorf("returned access token: got %q, want %q", got.AccessToken, tt.wantReturned)
			}
			if calls := f.calls.Load(); calls != tt.wantCalls {
				t.Errorf("token endpoint calls: got %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}

// TestReadPastStopsWithItsContext covers the bound on waiting for a lagging
// cache: a read that never moves past the rejected version ends when the
// refresh's own deadline does, rather than spinning on.
func TestReadPastStopsWithItsContext(t *testing.T) {
	f := newRefreshFixture(t, fixtureConfig{})
	seeded := f.seed(t, activeDelegation(time.Minute, oldRefresh))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := f.refresher.readPast(ctx, testKey, seeded.Version); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error: got %v, want the deadline", err)
	}
}

// TestTokenSharesOneRefreshAcrossCallers covers the singleflight. Callers that
// arrive while a refresh is in flight share it, and callers that arrive after
// find the refreshed record in the store, so either way the provider is asked
// once. Each caller also gets its own copy: they write to the scopes slice they
// were handed, which the race detector flags if any two share it.
func TestTokenSharesOneRefreshAcrossCallers(t *testing.T) {
	f := newRefreshFixture(t, fixtureConfig{
		respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
			time.Sleep(200 * time.Millisecond)
			return refreshedWith(nil)(f, form)
		},
	})
	f.seed(t, activeDelegation(time.Minute, oldRefresh))

	const callers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, callers)
	tokens := make([]string, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := f.refresher.Token(context.Background(), testKey)
			if err != nil {
				errs[i] = err
				return
			}
			tokens[i] = got.AccessToken
			got.Scopes[0] = "changed by caller"
		}()
	}
	close(start)
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Errorf("caller %d: %v", i, errs[i])
		} else if tokens[i] != newAccess {
			t.Errorf("caller %d got %q, want %q", i, tokens[i], newAccess)
		}
	}
	if calls := f.calls.Load(); calls != 1 {
		t.Errorf("token endpoint calls: got %d, want 1", calls)
	}
	if stored := f.stored(t); !slices.Equal(stored.Scopes, []string{"gist", "repo"}) {
		t.Errorf("stored scopes: got %v, a caller's copy leaked into the store", stored.Scopes)
	}
}

// TestTokenRefreshOutlivesItsCaller covers why the flight is detached. GitHub
// has already spent the old refresh token by the time it answers, so a refresh
// abandoned between that answer and the store write would lose the only
// working replacement.
func TestTokenRefreshOutlivesItsCaller(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	f := newRefreshFixture(t, fixtureConfig{
		respond: func(f *refreshFixture, form url.Values) (int, map[string]any) {
			close(entered)
			<-release
			return refreshedWith(nil)(f, form)
		},
	})
	f.seed(t, activeDelegation(time.Minute, oldRefresh))
	// Released on every path, including a failure below: the server's own
	// cleanup waits for its handlers, so a handler left blocked would hang
	// the test instead of failing it.
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandler)

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, err := f.refresher.Token(ctx, testKey)
		returned <- err
	}()

	<-entered
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled caller got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled caller was kept waiting on the refresh")
	}

	releaseHandler()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stored := f.stored(t); stored.AccessToken == newAccess {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the refresh was abandoned with its caller, so the new token was never stored")
}

// TestTokenRefusesARedirectedTokenEndpoint covers why the client does not
// follow redirects: Go re-sends a POST body on a 307, and this one carries the
// client secret and the refresh token.
func TestTokenRefusesARedirectedTokenEndpoint(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	f := newRefreshFixture(t, fixtureConfig{
		objects: func(string) []client.Object {
			return []client.Object{readyProvider(redirector.URL), usableSecret()}
		},
	})
	seeded := f.seed(t, activeDelegation(time.Minute, oldRefresh))

	if _, err := f.refresher.Token(context.Background(), testKey); err == nil ||
		!strings.Contains(err.Error(), "307") {
		t.Fatalf("error: got %v, want the redirect refused", err)
	}
	if hits := elsewhere.Load(); hits != 0 {
		t.Errorf("the redirect target received %d requests, want 0", hits)
	}
	if stored := f.stored(t); stored.Version != seeded.Version {
		t.Error("a refused refresh rewrote the delegation")
	}
}

func TestGrantedScopes(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]any
		want  []string
	}{
		{name: "absent", want: nil},
		{name: "empty", extra: map[string]any{"scope": ""}, want: nil},
		{name: "space separated, as RFC 6749 writes it",
			extra: map[string]any{"scope": "repo read:user"}, want: []string{"read:user", "repo"}},
		{name: "comma separated, as GitHub writes it",
			extra: map[string]any{"scope": "repo,read:user"}, want: []string{"read:user", "repo"}},
		{name: "repeats and stray separators", extra: map[string]any{"scope": " repo,, repo gist "},
			want: []string{"gist", "repo"}},
		{name: "not a string", extra: map[string]any{"scope": 42}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := (&oauth2.Token{AccessToken: newAccess}).WithExtra(tt.extra)
			if got := grantedScopes(token); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
