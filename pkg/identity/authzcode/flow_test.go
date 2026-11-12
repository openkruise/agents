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

package authzcode

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// provider is a third-party authorization server that checks what a real one
// checks.
//
// The point of testing against this rather than against the package is that it
// recomputes the PKCE challenge itself, from the verifier it is handed at the
// token endpoint. If the derivation here and the derivation in authzcode
// disagreed, every unit test would still pass and only a real provider would
// reject the exchange, which is the failure this is built to catch.
type provider struct {
	mu sync.Mutex
	// issued maps an authorization code to what the authorize request pledged.
	issued map[string]pledge
	// redeemed records codes already exchanged, so a replay can be refused the
	// way a real provider refuses one.
	redeemed map[string]bool
}

type pledge struct {
	challenge string
	method    string
	state     string
}

func newProvider() *provider {
	return &provider{issued: map[string]pledge{}, redeemed: map[string]bool{}}
}

func (p *provider) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", p.authorize)
	mux.HandleFunc("/token", p.token)
	return mux
}

// authorize stands in for the consent screen the user would see, then redirects
// to the callback the way the provider does.
func (p *provider) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	redirectURI := query.Get("redirect_uri")
	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}
	// A real provider rejects a request that pledges no challenge, or pledges
	// one under a method it does not implement.
	challenge := query.Get("code_challenge")
	method := query.Get("code_challenge_method")
	if challenge == "" || method != "S256" {
		http.Error(w, "unsupported code_challenge", http.StatusBadRequest)
		return
	}

	code := "code-" + strings.TrimPrefix(challenge, "")[:8]
	state := query.Get("state")

	p.mu.Lock()
	p.issued[code] = pledge{challenge: challenge, method: method, state: state}
	p.mu.Unlock()

	// The state comes back exactly as it was sent, which is the whole contract
	// the callback relies on.
	target, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	returned := target.Query()
	returned.Set("code", code)
	if state != "" {
		returned.Set("state", state)
	}
	target.RawQuery = returned.Encode()

	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusFound)
}

// token exchanges a code, verifying the PKCE pledge from scratch.
func (p *provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	code := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")

	p.mu.Lock()
	defer p.mu.Unlock()

	pledged, found := p.issued[code]
	if !found {
		writeOAuthError(w, "invalid_grant", "unknown code")
		return
	}
	if p.redeemed[code] {
		// Single use, enforced where it has to be: at the party holding the
		// state. authzcode cannot do this, which is why it says so.
		writeOAuthError(w, "invalid_grant", "code already redeemed")
		return
	}
	if verifier == "" {
		writeOAuthError(w, "invalid_request", "missing code_verifier")
		return
	}

	// Recomputed here independently of authzcode.
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != pledged.challenge {
		writeOAuthError(w, "invalid_grant", "code_verifier does not match code_challenge")
		return
	}

	p.redeemed[code] = true
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "gho_realAccessToken",
		"token_type":   "bearer",
		"scope":        "repo:status",
	})
}

func writeOAuthError(w http.ResponseWriter, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": description,
	})
}

// session is the subset of §6.4 this package's values live in. Only the digest
// and the verifier are kept; the state itself is deliberately absent.
// Fields are exported deliberately. fmt cannot call String on an unexported
// field, so a struct holding a Verifier unexported would print it in full
// under %v, whatever the type does. That is the caller contract the redaction
// note in authzcode.go describes, and this struct follows it.
type session struct {
	StateDigest StateDigest
	Verifier    Verifier
	Challenge   Challenge
	ExpiresAt   time.Time
}

// flow runs a provider and the control-plane side of the exchange against it.
type flow struct {
	provider *provider
	server   *httptest.Server
	client   *http.Client
	callback string
}

func startFlow(t *testing.T) *flow {
	t.Helper()

	p := newProvider()
	server := httptest.NewTLSServer(p.handler())
	t.Cleanup(server.Close)

	client := server.Client()
	// Redirects are inspected rather than followed: the callback is a separate
	// HTTP action the browser makes, and §6.4 is explicit that the provider
	// does not call it server to server.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client.Timeout = 10 * time.Second

	return &flow{
		provider: p,
		server:   server,
		client:   client,
		callback: "https://identity.example.com/oauth2/callback",
	}
}

// begin generates the two secrets, stores what a session stores, and sends the
// authorization request.
func (f *flow) begin(t *testing.T, lifetime time.Duration) (session, *url.URL) {
	t.Helper()

	state, digest, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	verifier, challenge, err := NewVerifier()
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	stored := session{
		StateDigest: digest,
		Verifier:    verifier,
		Challenge:   challenge,
		ExpiresAt:   Expiry(time.Now(), lifetime),
	}

	authorize, err := url.Parse(f.server.URL + "/authorize")
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	query := url.Values{}
	query.Set("response_type", "code")
	query.Set("client_id", "Iv1.0123456789abcdef")
	query.Set("redirect_uri", f.callback)
	query.Set("scope", "repo:status")
	query.Set("state", string(state))
	query.Set("code_challenge", string(challenge))
	query.Set("code_challenge_method", ChallengeMethod)
	authorize.RawQuery = query.Encode()

	response, err := f.client.Get(authorize.String())
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusFound {
		t.Fatalf("authorize returned %s, want a redirect", response.Status)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return stored, location
}

// exchange posts a code and verifier to the token endpoint.
func (f *flow) exchange(t *testing.T, code string, verifier Verifier) (int, map[string]any) {
	t.Helper()

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", f.callback)
	if verifier != "" {
		form.Set("code_verifier", string(verifier))
	}

	response, err := f.client.PostForm(f.server.URL+"/token", form)
	if err != nil {
		t.Fatalf("POST token: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	return response.StatusCode, body
}

// TestAuthorizationCodeFlowEndToEnd runs the whole round trip the package
// exists to secure, against a provider that verifies PKCE itself.
func TestAuthorizationCodeFlowEndToEnd(t *testing.T) {
	f := startFlow(t)
	stored, location := f.begin(t, DefaultLifetime)

	// The callback is a fresh request carrying whatever the provider put in the
	// query, so the state arrives as text that has survived URL encoding.
	returnedState := State(location.Query().Get("state"))
	code := location.Query().Get("code")
	if code == "" {
		t.Fatal("the provider returned no code")
	}

	if err := Validate(stored.StateDigest, returnedState, stored.ExpiresAt, time.Now()); err != nil {
		t.Fatalf("the state the provider echoed back did not validate: %v", err)
	}

	status, body := f.exchange(t, code, stored.Verifier)
	if status != http.StatusOK {
		t.Fatalf("token exchange returned %d: %v", status, body)
	}
	if body["access_token"] != "gho_realAccessToken" {
		t.Errorf("access_token is %v, want the provider's token", body["access_token"])
	}
}

// TestTheStateSurvivesAURLRoundTrip is the reason the generated values are
// restricted to the unreserved set.
//
// The state goes out in a query parameter and comes back in a different one, so
// a value needing escaping would survive or not depending on who encoded it.
// Comparing the digest after the round trip is what proves nothing was mangled.
func TestTheStateSurvivesAURLRoundTrip(t *testing.T) {
	f := startFlow(t)

	for i := 0; i < 64; i++ {
		stored, location := f.begin(t, DefaultLifetime)
		returned := State(location.Query().Get("state"))

		if strings.Contains(location.RawQuery, "%") && !strings.Contains(location.RawQuery, "%3A") {
			// Only the callback URL itself should need escaping, never the
			// state. A percent escape inside the state means it carried a
			// character the generator should not have produced.
			if escaped := url.QueryEscape(string(returned)); escaped != string(returned) {
				t.Fatalf("the state needed escaping: %q", string(returned))
			}
		}
		if !stored.StateDigest.Matches(returned) {
			t.Fatalf("iteration %d: the state did not survive the round trip", i)
		}
	}
}

// TestCallbackRejectsAnAttackersState covers the CSRF the state exists to stop,
// over the real round trip rather than against a constructed digest.
func TestCallbackRejectsAnAttackersState(t *testing.T) {
	f := startFlow(t)

	// The victim begins a flow.
	victim, victimLocation := f.begin(t, DefaultLifetime)
	victimCode := victimLocation.Query().Get("code")

	// The attacker begins their own, so they hold a genuine code and a genuine
	// state, just not the victim's.
	_, attackerLocation := f.begin(t, DefaultLifetime)
	attackerState := State(attackerLocation.Query().Get("state"))
	attackerCode := attackerLocation.Query().Get("code")

	cases := []struct {
		name    string
		state   State
		wantErr error
	}{
		{
			name:    "the attacker's own state against the victim's session",
			state:   attackerState,
			wantErr: ErrStateMismatch,
		},
		{
			// The easiest request to make: drop the parameter. This is the hole
			// the empty-state guard closes.
			name:    "no state at all",
			state:   "",
			wantErr: ErrStateMismatch,
		},
		{
			name:    "the stored digest presented as the state",
			state:   State(victim.StateDigest),
			wantErr: ErrStateMismatch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(victim.StateDigest, tc.state, victim.ExpiresAt, time.Now())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}

	// And the victim's own callback still works, so the rejections above are
	// not simply everything failing.
	if err := Validate(victim.StateDigest, State(victimLocation.Query().Get("state")),
		victim.ExpiresAt, time.Now()); err != nil {
		t.Fatalf("the victim's own callback was rejected: %v", err)
	}
	_ = victimCode
	_ = attackerCode
}

// TestAnInterceptedCodeIsUselessWithoutTheVerifier is what PKCE buys, proven by
// the provider refusing the exchange rather than by anything in this package.
func TestAnInterceptedCodeIsUselessWithoutTheVerifier(t *testing.T) {
	f := startFlow(t)
	stored, location := f.begin(t, DefaultLifetime)
	code := location.Query().Get("code")

	// An attacker who read the code out of the browser, a redirect chain or a
	// log has everything except the verifier, which never left this process.
	foreign, _, err := NewVerifier()
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	cases := []struct {
		name string
		// Exported for the same reason as session above: an unexported
		// Verifier would print in full under %v.
		Verifier Verifier
	}{
		{"no verifier", ""},
		{"a verifier of their own", foreign},
		{"the challenge replayed as the verifier", Verifier(stored.Challenge)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := f.exchange(t, code, tc.Verifier)
			if status == http.StatusOK {
				t.Fatalf("the provider accepted the exchange: %v", body)
			}
			if body["error"] == nil {
				t.Errorf("no OAuth error in the refusal: %v", body)
			}
		})
	}

	// The legitimate holder still succeeds, so the refusals are about the
	// verifier and not about the code having been spent.
	status, body := f.exchange(t, code, stored.Verifier)
	if status != http.StatusOK {
		t.Fatalf("the legitimate exchange failed with %d: %v", status, body)
	}
}

// TestACodeCannotBeRedeemedTwice shows where single use actually lives.
//
// authzcode documents that it cannot enforce this, and that the Pending
// transition has to be atomic. The provider refusing a replay is one half; the
// session store refusing one is the half still to be built in 3.6.
func TestACodeCannotBeRedeemedTwice(t *testing.T) {
	f := startFlow(t)
	stored, location := f.begin(t, DefaultLifetime)
	code := location.Query().Get("code")

	status, body := f.exchange(t, code, stored.Verifier)
	if status != http.StatusOK {
		t.Fatalf("the first exchange failed with %d: %v", status, body)
	}

	status, body = f.exchange(t, code, stored.Verifier)
	if status == http.StatusOK {
		t.Fatal("the same code was redeemed twice")
	}
	if body["error"] != "invalid_grant" {
		t.Errorf("replay error is %v, want invalid_grant", body["error"])
	}
}

// TestAnExpiredSessionIsRefusedBeforeTheCodeIsSpent covers the ordering over
// the real flow: a late callback must not reach the token endpoint at all.
func TestAnExpiredSessionIsRefusedBeforeTheCodeIsSpent(t *testing.T) {
	f := startFlow(t)

	// A session that has already closed by the time the callback arrives.
	stored, location := f.begin(t, time.Nanosecond)
	returned := State(location.Query().Get("state"))
	code := location.Query().Get("code")

	time.Sleep(time.Millisecond)

	err := Validate(stored.StateDigest, returned, stored.ExpiresAt, time.Now())
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("got %v, want ErrExpired", err)
	}

	// The code is still redeemable at the provider, which is exactly why the
	// expiry check has to happen on this side: nothing downstream will stop a
	// late callback from spending it.
	status, _ := f.exchange(t, code, stored.Verifier)
	if status != http.StatusOK {
		t.Errorf("the provider refused the code on its own, so this test proves nothing")
	}
}
