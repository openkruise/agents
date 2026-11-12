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

// Package jwtissuer mints the signed traffic access tokens that
// pkg/identity/oidc verifies, and publishes the public keys needed to verify
// them. It is the issuing half of the contract described in
// docs/proposals/20260713-traffic-access-token-jwt-verification.md.
package jwtissuer

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/openkruise/agents/pkg/identity/aead"
)

// DefaultTokenLifetime is the validity stamped on a traffic access token when
// the caller does not choose one.
const DefaultTokenLifetime = time.Hour

// DefaultPrincipalTokenLifetime is how long a principal token stays valid when
// a caller sets no validity.
//
// Three days, which is a different order of magnitude from the agent token
// above on purpose. A principal token spares the caller from re-presenting an
// ID Token on every credential request, and re-authenticating the user daily
// would defeat that. The ID Token must still be valid at the moment of
// exchange; this is not an extension of it.
//
// It is long enough to need a revocation story. Disabling a user at the IdP
// does not invalidate an already-issued principal token, so a deployment must
// either revoke by jti or accept a bounded delay. What is not delayed is
// authorization: every credential request rechecks the Sandbox, the
// AgentRoleBinding and the delegation, so a withdrawn delegation or a deleted
// binding fails closed at once.
const DefaultPrincipalTokenLifetime = 72 * time.Hour

// minRSAKeyBits is the smallest RSA modulus RFC 7518 section 3.3 permits for
// RS256.
const minRSAKeyBits = 2048

const (
	// AgentTokenType is the token_type claim on an agent token. An agent token
	// and a principal token may be signed by the same key, so a verifier that
	// checks only the signature would accept either wherever it expects one.
	// This claim is what makes them non-interchangeable.
	AgentTokenType = "agent"

	// AgentTokenAudience is the aud claim on an agent token. The identity
	// provider is the only intended consumer: a sandbox presents this token to
	// exchange it for a principal token, and nothing else should accept it.
	AgentTokenAudience = "agent-identity-provider"

	// PrincipalTokenType is the token_type claim on a principal token. It is
	// what stops a principal token being presented where an agent token is
	// expected, and the reverse, given both may be signed by the same key.
	PrincipalTokenType = "principal"

	// PrincipalTokenAudience is the aud claim on a principal token. The
	// credential provider is the only intended consumer: this token authorises
	// fetching a third-party credential the user delegated, so the identity
	// provider itself must not accept one.
	//
	// gosec reads the name and the value as a credential. It is the opposite of
	// one: an audience names the service a token is addressed to, it is public,
	// and it travels in the clear inside every principal token.
	PrincipalTokenAudience = "credential-provider" // #nosec G101 -- an audience name, not a credential
)

// SigningKey is the private key an Issuer signs with, paired with the key ID it
// publishes in the JWKS. The KeyID must be stable for as long as tokens signed
// by this key can still be presented.
type SigningKey struct {
	KeyID      string
	PrivateKey crypto.Signer
}

// VerificationKey is a public key published in the JWKS but no longer used for
// signing. Retaining the previous key here is what makes rotation safe.
//
// As the gateway behaves today it snapshots the JWKS once at startup and never
// refetches, so a new key must be published and rolled out before it signs
// anything. If the verifier gains a refresh, retention stops being the only
// thing preventing a fleet of 401s and becomes a window for verifiers that have
// not refreshed yet. Retaining the previous key is correct under both.
type VerificationKey struct {
	KeyID     string
	PublicKey crypto.PublicKey
}

// SandboxBinding identifies the sandbox a token is minted for. The gateway
// compares these against the route it selected, so a token for one sandbox
// cannot be replayed against another.
//
// Tokens carry no jti. They are short-lived bearer tokens bound to a single
// sandbox, and a jti would only be useful alongside a revocation store that
// nothing here maintains. The trade is that a leaked token stays valid for the
// rest of its lifetime; shortening the lifetime is the lever, or rotating the
// signing key and dropping the retained one to invalidate every outstanding
// token at once.
type SandboxBinding struct {
	SandboxID  string
	SandboxUID string
}

// Issuer mints tokens for a single issuer URL.
type Issuer struct {
	issuerURL string
	lifetime  time.Duration
	// principalLifetime is kept apart from lifetime because the two are
	// different orders of magnitude. An agent token is short-lived and proves
	// a workload is running now; a principal token stands in for an
	// authentication that happened once and is measured in days. Sharing one
	// default would force each to constrain the other.
	principalLifetime time.Duration

	// userInfoKeys seals the userinfo claim. It is deliberately not the
	// signing key: a verifier needs the public signing key to check the token,
	// and must not thereby be able to read the user identity inside it.
	userInfoKeys *aead.Keyring

	active   SigningKey
	retained []VerificationKey

	now func() time.Time
}

// Option adjusts an Issuer at construction time.
type Option func(*Issuer)

// WithTokenLifetime sets how long minted agent and traffic access tokens stay
// valid. It does not affect principal tokens; see WithPrincipalTokenLifetime.
func WithTokenLifetime(lifetime time.Duration) Option {
	return func(i *Issuer) { i.lifetime = lifetime }
}

// WithPrincipalTokenLifetime sets how long minted principal tokens stay valid.
//
// Separate from WithTokenLifetime because the two serve different purposes and
// a deployment will want very different numbers for them.
func WithPrincipalTokenLifetime(lifetime time.Duration) Option {
	return func(i *Issuer) { i.principalLifetime = lifetime }
}

// WithUserInfoKeyring supplies the keyring that seals the userinfo claim.
//
// Without it IssuePrincipalToken refuses to mint anything. A principal token
// whose userinfo were absent or in cleartext would either be unusable, because
// nothing could recover the user identity to find their delegation, or would
// publish that identity to every holder of the token.
func WithUserInfoKeyring(keyring *aead.Keyring) Option {
	return func(i *Issuer) { i.userInfoKeys = keyring }
}

// WithRetainedKeys publishes additional public keys that are no longer used for
// signing. Use this during a rotation overlap window so verifiers holding an
// older JWKS snapshot still accept tokens signed by the previous key.
func WithRetainedKeys(keys ...VerificationKey) Option {
	return func(i *Issuer) { i.retained = append(i.retained, keys...) }
}

// withClock overrides the time source. Tests use it to mint tokens outside the
// current validity window.
func withClock(now func() time.Time) Option {
	return func(i *Issuer) { i.now = now }
}

// New returns an Issuer that signs with active and publishes it under issuerURL.
// issuerURL must be an absolute HTTPS URL: it becomes the iss claim, and the
// verifier compares it against the issuer advertised by OIDC discovery.
func New(issuerURL string, active SigningKey, opts ...Option) (*Issuer, error) {
	if err := validateHTTPSURL(issuerURL); err != nil {
		return nil, fmt.Errorf("issuer URL: %w", err)
	}
	if active.KeyID == "" {
		return nil, fmt.Errorf("signing key ID must not be empty")
	}
	if active.PrivateKey == nil {
		return nil, fmt.Errorf("signing key must not be nil")
	}
	if _, err := signatureAlgorithmFor(active.PrivateKey.Public()); err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}

	issuer := &Issuer{
		issuerURL:         issuerURL,
		lifetime:          DefaultTokenLifetime,
		principalLifetime: DefaultPrincipalTokenLifetime,
		active:            active,
		now:               time.Now,
	}
	for _, opt := range opts {
		opt(issuer)
	}

	if issuer.lifetime <= 0 {
		return nil, fmt.Errorf("token lifetime must be positive")
	}
	if err := issuer.validateRetainedKeys(); err != nil {
		return nil, err
	}
	return issuer, nil
}

func (i *Issuer) validateRetainedKeys() error {
	seen := map[string]struct{}{i.active.KeyID: {}}
	for _, key := range i.retained {
		if key.KeyID == "" {
			return fmt.Errorf("retained key ID must not be empty")
		}
		if _, exists := seen[key.KeyID]; exists {
			return fmt.Errorf("duplicate key ID %q", key.KeyID)
		}
		if _, err := signatureAlgorithmFor(key.PublicKey); err != nil {
			return fmt.Errorf("retained key %q: %w", key.KeyID, err)
		}
		seen[key.KeyID] = struct{}{}
	}
	return nil
}

// IssueTrafficAccessToken mints a token binding subject to a single sandbox and
// returns it alongside its expiry, so callers can record the expiry without
// parsing the token back.
//
// validity overrides the issuer's default lifetime for this token; zero uses the
// default. The caller resolves validity as policy per request, so it cannot be
// fixed at construction: sandbox-manager normalizes TokenOptions.RequestedValidity
// before issuance and the value is not client-controlled.
func (i *Issuer) IssueTrafficAccessToken(subject string, binding SandboxBinding,
	validity time.Duration) (string, time.Time, error) {
	if subject == "" {
		return "", time.Time{}, fmt.Errorf("subject must not be empty")
	}
	if binding.SandboxID == "" {
		return "", time.Time{}, fmt.Errorf("sandbox ID must not be empty")
	}
	if binding.SandboxUID == "" {
		return "", time.Time{}, fmt.Errorf("sandbox UID must not be empty")
	}

	signer, err := i.newSigner()
	if err != nil {
		return "", time.Time{}, err
	}

	// The verifier requires exp, iat and nbf to all be present.
	issuedAt, expiry := i.window(validity)
	claims := trafficAccessTokenClaims{
		Claims: jwt.Claims{
			Issuer:    i.issuerURL,
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			NotBefore: jwt.NewNumericDate(issuedAt),
			Expiry:    jwt.NewNumericDate(expiry),
		},
		Sandbox: sandboxClaims{
			SandboxID:  binding.SandboxID,
			SandboxUID: binding.SandboxUID,
		},
	}

	rawJWT, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return rawJWT, expiry, nil
}

// AgentBinding identifies the workload an agent token is minted for.
//
// Every field is read by the control plane from the Sandbox and its
// AgentIdentity. None of it may come from the body of an issuance request: a
// caller that could name its own namespace or agent identity could mint a token
// for an agent it does not run.
type AgentBinding struct {
	Namespace         string
	AgentIdentityName string
	// SandboxUID identifies the workload rather than the sandbox name, because a
	// name can be reused and a UID cannot.
	SandboxUID string
}

// IssueAgentToken mints a token proving which workload is calling, and returns
// it alongside its expiry so callers can record the expiry without parsing the
// token back.
//
// This is a different credential from IssueTrafficAccessToken, not a variant of
// it. That one lets a client into a sandbox through the gateway; this one lets a
// sandbox prove to the identity provider which agent it is running. They may
// share a signing key, so token_type and a distinct audience are what stop one
// being presented where the other is expected.
//
// validity of zero uses the issuer default.
func (i *Issuer) IssueAgentToken(binding AgentBinding, validity time.Duration) (string, time.Time, error) {
	if binding.Namespace == "" {
		return "", time.Time{}, fmt.Errorf("namespace must not be empty")
	}
	if binding.AgentIdentityName == "" {
		return "", time.Time{}, fmt.Errorf("agent identity name must not be empty")
	}
	if binding.SandboxUID == "" {
		return "", time.Time{}, fmt.Errorf("sandbox UID must not be empty")
	}

	signer, err := i.newSigner()
	if err != nil {
		return "", time.Time{}, err
	}

	issuedAt, expiry := i.window(validity)

	claims := agentTokenClaims{
		Claims: jwt.Claims{
			Issuer: i.issuerURL,
			// The subject names the logical agent, not the sandbox running it.
			// Delegations are isolated by namespace and agent identity, so two
			// sandboxes of the same agent share a subject and a delegation, while
			// the same sandbox under a different identity does not. The sandbox is
			// identified by sandbox_uid, which is what binds a token to one
			// workload.
			Subject:   fmt.Sprintf("agent:%s:%s", binding.Namespace, binding.AgentIdentityName),
			Audience:  jwt.Audience{AgentTokenAudience},
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			NotBefore: jwt.NewNumericDate(issuedAt),
			Expiry:    jwt.NewNumericDate(expiry),
		},
		TokenType:         AgentTokenType,
		Namespace:         binding.Namespace,
		AgentIdentityName: binding.AgentIdentityName,
		SandboxUID:        binding.SandboxUID,
	}

	rawJWT, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign agent token: %w", err)
	}
	return rawJWT, expiry, nil
}

// agentTokenClaims carries the workload identity the control plane derived.
// Claim names follow the proposal rather than the camelCase the traffic access
// token uses, because this token has a different consumer.
type agentTokenClaims struct {
	jwt.Claims
	TokenType         string `json:"token_type"`
	Namespace         string `json:"namespace"`
	AgentIdentityName string `json:"agent_identity_name"`
	SandboxUID        string `json:"sandbox_uid"`
}

// PrincipalBinding identifies the user a principal token speaks for and the
// agent acting on their behalf.
//
// Issuer and Subject must come from an inbound credential this process has
// already verified. The proposal is explicit that the principal is derived and
// never passed: a user identity a caller can populate freely is an assertion
// rather than an authentication, so anything able to request issuance could
// otherwise name an arbitrary user and reach the credentials that user
// delegated.
type PrincipalBinding struct {
	// IDToken is the raw ID Token this process has already verified. It is
	// sealed into the userinfo claim so the service can recover the user
	// identity later, which is what a delegation is keyed by.
	//
	// It is not re-verified here. Issuing is not the boundary that checks it;
	// by the time a binding reaches this function the caller must have
	// validated the signature, issuer, audience and expiry, and these fields
	// are what it read out of doing so.
	IDToken string

	// Issuer is the iss claim of that verified token. Carried alongside the
	// token so the service can read it without parsing the JWT again, and
	// because a subject is only unique within its issuer: two providers may
	// both call a user "1234".
	Issuer string

	// Subject is the sub claim of that same verified token.
	Subject string

	// Agent is the workload acting for the user, taken from its own verified
	// agent token rather than from the request.
	Agent AgentBinding
}

// userInfo is the sealed payload of the userinfo claim.
//
// The issuer and subject are stored beside the token so a consumer can find a
// delegation without parsing the JWT again, and the token itself is kept so
// anything else about that authentication remains recoverable.
type userInfo struct {
	Issuer  string `json:"iss"`
	Subject string `json:"sub"`
	IDToken string `json:"id_token"`
}

// IssuePrincipalToken mints a token proving which user an agent is acting for,
// and returns it alongside its expiry.
//
// This is what the exchange an agent token buys produces: the sandbox proves
// which agent it runs, the identity provider verifies the user inbound
// credential, and the result is a token the credential provider accepts for
// that user and that agent together. Neither half alone is enough, which is
// what act carries.
//
// validity of zero uses the issuer default.
func (i *Issuer) IssuePrincipalToken(binding PrincipalBinding,
	validity time.Duration) (string, time.Time, error) {
	if binding.Issuer == "" {
		return "", time.Time{}, fmt.Errorf("verified credential issuer must not be empty")
	}
	if binding.Subject == "" {
		return "", time.Time{}, fmt.Errorf("verified credential subject must not be empty")
	}
	if binding.Agent.Namespace == "" {
		return "", time.Time{}, fmt.Errorf("namespace must not be empty")
	}
	if binding.Agent.AgentIdentityName == "" {
		return "", time.Time{}, fmt.Errorf("agent identity name must not be empty")
	}
	if binding.Agent.SandboxUID == "" {
		return "", time.Time{}, fmt.Errorf("sandbox UID must not be empty")
	}
	if binding.IDToken == "" {
		return "", time.Time{}, fmt.Errorf("verified ID token must not be empty")
	}
	if i.userInfoKeys == nil {
		// Refused rather than minting a token with no userinfo. Such a token
		// would verify and then be useless, because nothing could recover the
		// user identity needed to find their delegation.
		return "", time.Time{}, fmt.Errorf(
			"no userinfo keyring configured; principal tokens cannot be issued without one")
	}

	signer, err := i.newSigner()
	if err != nil {
		return "", time.Time{}, err
	}

	// A fresh identifier per issuance. Nothing looks a delegation up by it, so
	// it does not need to be stable; it exists to name this one token, which
	// is what a revocation list would name.
	tokenID, err := randomTokenID()
	if err != nil {
		return "", time.Time{}, err
	}

	// The subject identifies this token, not the user. The user is in userinfo,
	// sealed. Deriving sub from the user identity is what previously forced it
	// to be a one-way hash and made the identity unrecoverable.
	subject, err := randomTokenID()
	if err != nil {
		return "", time.Time{}, err
	}

	// Sealed under the token's own identifier, so a userinfo blob lifted out of
	// one token and pasted into another fails to open.
	sealed, err := i.userInfoKeys.Seal(mustEncodeUserInfo(binding), userInfoContext, tokenID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("seal userinfo: %w", err)
	}

	issuedAt, expiry := i.principalWindow(validity)

	claims := principalTokenClaims{
		Claims: jwt.Claims{
			Issuer:    i.issuerURL,
			Subject:   subject,
			ID:        tokenID,
			Audience:  jwt.Audience{PrincipalTokenAudience},
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			NotBefore: jwt.NewNumericDate(issuedAt),
			Expiry:    jwt.NewNumericDate(expiry),
		},
		TokenType: PrincipalTokenType,
		UserInfo:  base64.RawURLEncoding.EncodeToString(sealed),
		Actor: actorClaim{
			// namespace and agent_identity_name are separate claims rather
			// than parts of a formatted subject. The delegation key consumes
			// both directly, and making a consumer split "agent:<ns>:<name>"
			// back apart would turn a formatting choice into a contract that
			// breaks silently if the separator ever changes.
			Namespace:         binding.Agent.Namespace,
			AgentIdentityName: binding.Agent.AgentIdentityName,
			SandboxUID:        binding.Agent.SandboxUID,
			// Kept as well, so a verifier comparing this against the agent
			// token's own subject does not have to rebuild it.
			Subject: fmt.Sprintf("agent:%s:%s",
				binding.Agent.Namespace, binding.Agent.AgentIdentityName),
		},
	}

	rawJWT, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign principal token: %w", err)
	}
	return rawJWT, expiry, nil
}

// principalTokenClaims carries the user the token speaks for and the agent
// acting for them.
type principalTokenClaims struct {
	jwt.Claims
	TokenType string `json:"token_type"`
	// UserInfo is the sealed user identity, base64url encoded. Only the
	// service holding the userinfo key can read it; a verifier checking the
	// signature cannot.
	UserInfo string `json:"userinfo"`
	// Actor is the act claim from RFC 8693 section 4.1, which exists for
	// exactly this shape: a token whose subject is one party while another
	// party is acting. The RFC permits additional members, so the sandbox
	// travels inside it rather than at the top level, where it would read as a
	// property of the user.
	Actor actorClaim `json:"act"`
}

// actorClaim names the party acting on the subject behalf.
type actorClaim struct {
	Subject string `json:"sub"`
	// Named to match agentTokenClaims, so the same value has the same name
	// wherever it appears.
	Namespace         string `json:"namespace"`
	AgentIdentityName string `json:"agent_identity_name"`
	SandboxUID        string `json:"sandbox_uid"`
}

// userInfoContext is the first associated-data part every userinfo seal
// carries, so a blob from this claim cannot be opened as anything else.
const userInfoContext = "principal-userinfo"

// principalWindow resolves a principal token's validity, falling back to the
// principal default rather than the shared one.
func (i *Issuer) principalWindow(validity time.Duration) (issuedAt, expiry time.Time) {
	if validity <= 0 {
		validity = i.principalLifetime
	}
	issuedAt = i.now()
	return issuedAt, issuedAt.Add(validity)
}

// randomTokenID returns 128 bits of randomness as unpadded base64url.
//
// Enough that a jti cannot be guessed, which matters if a deployment ever
// revokes by jti: a guessable identifier would let somebody revoke a token
// they never saw.
func randomTokenID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buffer); err != nil {
		return "", fmt.Errorf("generate token identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// mustEncodeUserInfo encodes the user identity for sealing.
//
// The error is impossible for three strings and is dropped rather than
// returned, because a json.Marshal error here would otherwise have to be
// wrapped with the value that failed, and that value is the user identity.
func mustEncodeUserInfo(binding PrincipalBinding) []byte {
	encoded, err := json.Marshal(userInfo{
		Issuer:  binding.Issuer,
		Subject: binding.Subject,
		IDToken: binding.IDToken,
	})
	if err != nil {
		return nil
	}
	return encoded
}

// OpenUserInfo decrypts the userinfo claim of a principal token.
//
// tokenID is the token's jti, which the claim was sealed under: passing the
// wrong one fails rather than returning somebody else's identity. The caller
// must have verified the token's signature first, because this says nothing
// about whether the surrounding token is genuine.
func (i *Issuer) OpenUserInfo(encoded, tokenID string) (issuer, subject, idToken string, err error) {
	if i.userInfoKeys == nil {
		return "", "", "", fmt.Errorf("no userinfo keyring configured")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", "", fmt.Errorf("decode userinfo")
	}
	plaintext, err := i.userInfoKeys.Open(sealed, userInfoContext, tokenID)
	if err != nil {
		return "", "", "", fmt.Errorf("open userinfo: %w", err)
	}
	var info userInfo
	if err := json.Unmarshal(plaintext, &info); err != nil {
		return "", "", "", fmt.Errorf("decode userinfo")
	}
	return info.Issuer, info.Subject, info.IDToken, nil
}

// window resolves the validity for one issuance and returns when the token is
// issued alongside when it expires.
//
// A validity of zero or less falls back to the issuer default, since the caller
// resolves validity as per-request policy and may have none to pass. nbf is set
// equal to iat by both callers rather than backdated: oidc.DefaultClockSkew is
// one minute, so a replica running slightly ahead of a gateway does not trip
// nbf.
func (i *Issuer) window(validity time.Duration) (issuedAt, expiry time.Time) {
	if validity <= 0 {
		validity = i.lifetime
	}
	issuedAt = i.now()
	return issuedAt, issuedAt.Add(validity)
}

// newSigner builds a signer over the active key. Shared by both token kinds so
// a change to the algorithm choice or the kid header cannot apply to one and
// miss the other.
func (i *Issuer) newSigner() (jose.Signer, error) {
	algorithm, err := signatureAlgorithmFor(i.active.PrivateKey.Public())
	if err != nil {
		return nil, err
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: algorithm, Key: i.active.PrivateKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", i.active.KeyID),
	)
	if err != nil {
		return nil, fmt.Errorf("create signer: %w", err)
	}
	return signer, nil
}

// trafficAccessTokenClaims mirrors oidc.TrafficAccessTokenClaims. It is
// duplicated rather than imported so the issuer and the verifier stay
// independently changeable; the round-trip test is what keeps them aligned.
type trafficAccessTokenClaims struct {
	jwt.Claims
	Sandbox sandboxClaims `json:"sandbox"`
}

type sandboxClaims struct {
	SandboxID  string `json:"sandboxId"`
	SandboxUID string `json:"sandboxUid"`
}

// PublicKeySet returns the keys to publish at the JWKS endpoint: the active
// signing key first, then any retained keys.
func (i *Issuer) PublicKeySet() (jose.JSONWebKeySet, error) {
	keys := make([]jose.JSONWebKey, 0, 1+len(i.retained))

	activeKey, err := publicJWK(i.active.KeyID, i.active.PrivateKey.Public())
	if err != nil {
		return jose.JSONWebKeySet{}, err
	}
	keys = append(keys, activeKey)

	for _, retained := range i.retained {
		key, err := publicJWK(retained.KeyID, retained.PublicKey)
		if err != nil {
			return jose.JSONWebKeySet{}, err
		}
		keys = append(keys, key)
	}
	return jose.JSONWebKeySet{Keys: keys}, nil
}

func publicJWK(keyID string, publicKey crypto.PublicKey) (jose.JSONWebKey, error) {
	algorithm, err := signatureAlgorithmFor(publicKey)
	if err != nil {
		return jose.JSONWebKey{}, fmt.Errorf("key %q: %w", keyID, err)
	}
	return jose.JSONWebKey{
		Key:       publicKey,
		KeyID:     keyID,
		Use:       "sig",
		Algorithm: string(algorithm),
	}, nil
}

// signatureAlgorithmFor picks the JWS algorithm for a key type. The verifier
// rejects a token whose algorithm does not match its key, so this is the single
// place that decides the pairing.
func signatureAlgorithmFor(publicKey crypto.PublicKey) (jose.SignatureAlgorithm, error) {
	switch key := publicKey.(type) {
	case *rsa.PublicKey:
		// RFC 7518 section 3.3 requires at least 2048 bits for RS256, and neither
		// go-jose nor the gateway verifier enforces it: oidc.algorithmSupportsKey
		// pairs on key type and curve size and never looks at the RSA modulus. The
		// issuer is the only side that can decline to create the problem, so a weak
		// key is refused here rather than producing tokens nothing flags.
		if bits := key.N.BitLen(); bits < minRSAKeyBits {
			return "", fmt.Errorf("RSA key is %d bits, RS256 requires at least %d", bits, minRSAKeyBits)
		}
		return jose.RS256, nil
	case *ecdsa.PublicKey:
		switch bitSize := key.Curve.Params().BitSize; bitSize {
		case 256:
			return jose.ES256, nil
		case 384:
			return jose.ES384, nil
		case 521:
			return jose.ES512, nil
		default:
			return "", fmt.Errorf("unsupported ECDSA curve size %d", bitSize)
		}
	case ed25519.PublicKey:
		return jose.EdDSA, nil
	default:
		return "", fmt.Errorf("unsupported key type %T", publicKey)
	}
}

func validateHTTPSURL(rawURL string) error {
	parsedURL, err := url.Parse(rawURL)
	if err != nil || !parsedURL.IsAbs() || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return fmt.Errorf("must be an absolute HTTPS URL")
	}
	return nil
}
