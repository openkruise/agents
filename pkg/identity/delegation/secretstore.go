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
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/identity/aead"
)

// Secret types, so the two kinds of record are distinguishable at a glance and
// an RBAC rule or an audit query can tell them apart. Section 8.2 asks for
// stable types and labels.
const (
	SessionSecretType    corev1.SecretType = "identity.agents.kruise.io/oauth-session"
	DelegationSecretType corev1.SecretType = "identity.agents.kruise.io/user-delegation"
)

// Labels carried on every record. The values are addresses, never secrets: a
// principal ID is already opaque and an agent identity is a resource name.
const (
	LabelRecordKind         = "identity.agents.kruise.io/record"
	LabelAgentIdentity      = "identity.agents.kruise.io/agent-identity"
	LabelCredentialProvider = "identity.agents.kruise.io/credential-provider"
)

// payloadKey is the single Secret data key holding the sealed record.
//
// One opaque blob rather than a field per value, because the fields are sealed
// together under associated data that binds them to this address. Splitting
// them would invite sealing each on its own, and a record whose fields can be
// mixed and matched from different records is not bound to anything.
const payloadKey = "payload"

// namePrefix keeps the two kinds apart in the namespace and makes a stray
// record recognisable.
const (
	sessionNamePrefix    = "oauth-session-"
	delegationNamePrefix = "user-delegation-"
)

// secretStore is what the two stores share: a client, a keyring and the one
// namespace every record lives in.
//
// Every record is sealed before it is written, so a reader of the Secret sees
// ciphertext. The root key is held in a file mount and never in a Secret
// alongside these, because a key reachable by the same RBAC grant as the
// ciphertext protects nothing.
//
// The namespace is a deployment decision rather than a property of a record:
// sessions and delegations are kept together in sandbox-system so one RBAC
// rule covers them.
type secretStore struct {
	client    client.Client
	keyring   *aead.Keyring
	namespace string
}

// SecretSessionStore persists OAuth sessions as Kubernetes Secrets.
type SecretSessionStore struct{ secretStore }

// SecretDelegationStore persists user delegations as Kubernetes Secrets.
//
// Separate from the session store rather than one type with both sets of
// methods: section 8.2 keeps the two kinds of record apart, their keys are
// different shapes, and a caller that only refreshes delegations has no
// business holding a handle that can claim sessions.
type SecretDelegationStore struct{ secretStore }

// NewSecretSessionStore returns a session store writing into namespace.
func NewSecretSessionStore(c client.Client, keyring *aead.Keyring, namespace string) (*SecretSessionStore, error) {
	base, err := newSecretStore(c, keyring, namespace)
	if err != nil {
		return nil, err
	}
	return &SecretSessionStore{base}, nil
}

// NewSecretDelegationStore returns a delegation store writing into namespace.
func NewSecretDelegationStore(c client.Client, keyring *aead.Keyring, namespace string) (*SecretDelegationStore, error) {
	base, err := newSecretStore(c, keyring, namespace)
	if err != nil {
		return nil, err
	}
	return &SecretDelegationStore{base}, nil
}

func newSecretStore(c client.Client, keyring *aead.Keyring, namespace string) (secretStore, error) {
	if c == nil {
		return secretStore{}, fmt.Errorf("delegation: a client is required")
	}
	if keyring == nil {
		// Refused rather than defaulted to storing cleartext. A store that
		// silently wrote unsealed refresh tokens would be worse than one that
		// would not start.
		return secretStore{}, fmt.Errorf("delegation: a keyring is required; records are never stored unsealed")
	}
	if namespace == "" {
		return secretStore{}, fmt.Errorf("delegation: a namespace is required")
	}
	return secretStore{client: c, keyring: keyring, namespace: namespace}, nil
}

var (
	_ SessionStore    = (*SecretSessionStore)(nil)
	_ DelegationStore = (*SecretDelegationStore)(nil)
)

// sessionPayload is the sealed part of a session. The fields outside it are
// the ones a lookup or an expiry sweep needs without decrypting.
type sessionPayload struct {
	Verifier           string    `json:"verifier"`
	AgentIdentity      string    `json:"agentIdentity"`
	SandboxName        string    `json:"sandboxName"`
	SandboxUID         string    `json:"sandboxUid"`
	UserIssuer         string    `json:"userIssuer"`
	UserSubject        string    `json:"userSubject"`
	CredentialProvider string    `json:"credentialProvider"`
	Scopes             []string  `json:"scopes"`
	RedirectURI        string    `json:"redirectUri"`
	CreatedAt          time.Time `json:"createdAt"`
	ExpiresAt          time.Time `json:"expiresAt"`
	Status             string    `json:"status"`
}

// delegationPayload is the sealed part of a delegation.
type delegationPayload struct {
	AccessToken          string    `json:"accessToken"`
	RefreshToken         string    `json:"refreshToken"`
	ExternalAccountID    string    `json:"externalAccountId"`
	Scopes               []string  `json:"scopes"`
	AccessTokenExpiresAt time.Time `json:"accessTokenExpiresAt"`
	State                string    `json:"state"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

// SessionSecretName is the Secret name for a session key.
//
// Derived rather than composed, because a state digest is base64url and a
// namespace can be up to 253 characters, so neither fits a name directly. The
// parts are length-prefixed before hashing for the same reason the principal ID
// is: concatenation would let two different addresses collide, and a collision
// here means one session overwriting another.
func SessionSecretName(key SessionKey) string {
	return sessionNamePrefix + digestOf(key.Namespace, key.StateDigest)
}

// DelegationSecretName is the Secret name for a delegation key.
//
// All five parts of section 8.1 go into the digest. Dropping any of them would
// merge delegations the proposal requires to stay separate, most consequentially
// AgentIdentity: two agents acting for the same user would share one
// authorization.
//
// The issuer is normalized first, so the same provider written with and without
// a trailing slash resolves to one delegation rather than two. Nothing else is
// folded: case, percent-encoding and default ports are left as the provider
// sent them, because treating two spellings as equal is how a check like this
// gets bypassed, and the asymmetry decides it. Folding too much merges two
// users; folding too little only makes one user authorize again.
//
// Raw user identifiers never appear in the name. The digest is what is stored,
// which is what section 8.1 means by this being a logical key.
func DelegationSecretName(key DelegationKey) string {
	return delegationNamePrefix + digestOf(
		key.Namespace, normalizeIssuer(key.UserIssuer), key.UserSubject,
		key.AgentIdentity, key.CredentialProvider)
}

// normalizeIssuer removes a trailing slash from an issuer identifier and
// changes nothing else.
//
// The reason is stability rather than tidiness: a delegation is stored against
// the derived value, so if the same provider is seen once as
// https://idp.example/realms/agents and once with a trailing slash, the user
// loses access to a delegation they already granted.
func normalizeIssuer(issuer string) string {
	return strings.TrimSuffix(issuer, "/")
}

// digestOf hashes length-prefixed parts into an unpadded base64url string.
func digestOf(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		digest.Write(length[:])
		digest.Write([]byte(part))
	}
	// Lowercased because a Secret name must be a DNS subdomain, and base64url
	// is mixed case. Lowercasing folds two digests together only with
	// probability far below the collision rate of the hash itself.
	return toDNS(base64.RawURLEncoding.EncodeToString(digest.Sum(nil)))
}

// toDNS maps base64url output into the lowercase alphanumeric set a Secret
// name allows, leaving "-" alone and replacing "_".
func toDNS(encoded string) string {
	out := make([]byte, 0, len(encoded))
	for i := 0; i < len(encoded); i++ {
		c := encoded[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c == '_':
			out = append(out, '-')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// sessionContext is the associated data a session is sealed under.
//
// Binding the address into the seal is what stops a record being moved: a
// session Secret copied to another namespace, or relabelled for another state,
// fails to open rather than decrypting into a session that looks legitimate.
func sessionContext(key SessionKey) []string {
	return []string{"session", key.Namespace, key.StateDigest}
}

// delegationContext is the associated data a delegation is sealed under, for
// the same reason, over all four parts of its key.
func delegationContext(key DelegationKey) []string {
	return []string{
		"delegation", key.Namespace, normalizeIssuer(key.UserIssuer), key.UserSubject,
		key.AgentIdentity, key.CredentialProvider,
	}
}

func (s *SecretSessionStore) Create(ctx context.Context, session *Session) error {
	if err := validateSessionKey(session.Key); err != nil {
		return err
	}
	payload := sessionPayload{
		Verifier:           session.Verifier,
		AgentIdentity:      session.AgentIdentity,
		SandboxName:        session.SandboxName,
		SandboxUID:         session.SandboxUID,
		UserIssuer:         session.UserIssuer,
		UserSubject:        session.UserSubject,
		CredentialProvider: session.CredentialProvider,
		Scopes:             session.Scopes,
		RedirectURI:        session.RedirectURI,
		CreatedAt:          session.CreatedAt,
		ExpiresAt:          session.ExpiresAt,
		// A session is always created Pending whatever the caller set, because
		// creating one already Exchanging would skip the single-use check.
		Status: string(StatusPending),
	}
	sealed, err := s.seal(payload, sessionContext(session.Key)...)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SessionSecretName(session.Key),
			Namespace: s.namespace,
			Labels: map[string]string{
				LabelRecordKind:         "oauth-session",
				LabelAgentIdentity:      session.AgentIdentity,
				LabelCredentialProvider: session.CredentialProvider,
			},
		},
		Type: SessionSecretType,
		Data: map[string][]byte{payloadKey: sealed},
	}
	if err := s.client.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("%w: a session already exists for this state", ErrConflict)
		}
		return fmt.Errorf("delegation: create session: %w", err)
	}
	session.Status = StatusPending
	session.Version = secret.ResourceVersion
	return nil
}

func (s *SecretSessionStore) Get(ctx context.Context, key SessionKey) (*Session, error) {
	if err := validateSessionKey(key); err != nil {
		return nil, err
	}
	secret, payload, err := s.readSession(ctx, key)
	if err != nil {
		return nil, err
	}
	return sessionFrom(key, secret.ResourceVersion, payload), nil
}

func (s *SecretSessionStore) BeginExchange(ctx context.Context, key SessionKey, now time.Time) (*Session, error) {
	if err := validateSessionKey(key); err != nil {
		return nil, err
	}
	secret, payload, err := s.readSession(ctx, key)
	if err != nil {
		return nil, err
	}

	// Expiry first. Answering "the state was right but you are too late"
	// confirms a guessed state to whoever asked.
	if !now.Before(payload.ExpiresAt) {
		return nil, ErrExpired
	}
	if SessionStatus(payload.Status) != StatusPending {
		return nil, ErrNotPending
	}

	payload.Status = string(StatusExchanging)
	sealed, err := s.seal(payload, sessionContext(key)...)
	if err != nil {
		return nil, err
	}
	secret.Data[payloadKey] = sealed

	// The update carries the resourceVersion the read returned, so two
	// callbacks racing the same state cannot both pass: the API server accepts
	// one and rejects the other with a conflict. This is the single-use
	// guarantee, and it is the API server enforcing it rather than this code.
	if err := s.client.Update(ctx, secret); err != nil {
		if apierrors.IsConflict(err) {
			// Another callback claimed it between the read and the write. From
			// the caller's side that is indistinguishable from finding it
			// already claimed, and must stay that way.
			return nil, ErrNotPending
		}
		return nil, fmt.Errorf("delegation: claim session: %w", err)
	}
	return sessionFrom(key, secret.ResourceVersion, payload), nil
}

func (s *SecretSessionStore) Finish(ctx context.Context, session *Session, status SessionStatus) error {
	if status != StatusCompleted && status != StatusFailed {
		return fmt.Errorf("delegation: %q is not a terminal session status", status)
	}
	if err := validateSessionKey(session.Key); err != nil {
		return err
	}

	secret, payload, err := s.readSession(ctx, session.Key)
	if err != nil {
		return err
	}
	payload.Status = string(status)
	sealed, err := s.seal(payload, sessionContext(session.Key)...)
	if err != nil {
		return err
	}
	secret.Data[payloadKey] = sealed
	if session.Version != "" {
		secret.ResourceVersion = session.Version
	}
	if err := s.client.Update(ctx, secret); err != nil {
		if apierrors.IsConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("delegation: finish session: %w", err)
	}
	session.Status = status
	session.Version = secret.ResourceVersion
	return nil
}

func (s *SecretSessionStore) Delete(ctx context.Context, key SessionKey) error {
	if err := validateSessionKey(key); err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SessionSecretName(key), Namespace: s.namespace},
	}
	if err := client.IgnoreNotFound(s.client.Delete(ctx, secret)); err != nil {
		return fmt.Errorf("delegation: delete session: %w", err)
	}
	return nil
}

func (s *SecretSessionStore) readSession(ctx context.Context, key SessionKey) (*corev1.Secret, sessionPayload, error) {
	secret := &corev1.Secret{}
	name := client.ObjectKey{Namespace: s.namespace, Name: SessionSecretName(key)}
	if err := s.client.Get(ctx, name, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, sessionPayload{}, ErrNotFound
		}
		return nil, sessionPayload{}, fmt.Errorf("delegation: read session: %w", err)
	}

	var payload sessionPayload
	if err := s.open(secret.Data[payloadKey], &payload, sessionContext(key)...); err != nil {
		return nil, sessionPayload{}, err
	}
	return secret, payload, nil
}

func sessionFrom(key SessionKey, version string, payload sessionPayload) *Session {
	return &Session{
		Key:                key,
		Verifier:           payload.Verifier,
		AgentIdentity:      payload.AgentIdentity,
		SandboxName:        payload.SandboxName,
		SandboxUID:         payload.SandboxUID,
		UserIssuer:         payload.UserIssuer,
		UserSubject:        payload.UserSubject,
		CredentialProvider: payload.CredentialProvider,
		Scopes:             payload.Scopes,
		RedirectURI:        payload.RedirectURI,
		CreatedAt:          payload.CreatedAt,
		ExpiresAt:          payload.ExpiresAt,
		Status:             SessionStatus(payload.Status),
		Version:            version,
	}
}

func (s *SecretDelegationStore) Get(ctx context.Context, key DelegationKey) (*Delegation, error) {
	if err := validateDelegationKey(key); err != nil {
		return nil, err
	}
	secret := &corev1.Secret{}
	name := client.ObjectKey{Namespace: s.namespace, Name: DelegationSecretName(key)}
	if err := s.client.Get(ctx, name, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("delegation: read delegation: %w", err)
	}

	var payload delegationPayload
	if err := s.open(secret.Data[payloadKey], &payload, delegationContext(key)...); err != nil {
		return nil, err
	}
	return &Delegation{
		Key:                  key,
		AccessToken:          payload.AccessToken,
		RefreshToken:         payload.RefreshToken,
		ExternalAccountID:    payload.ExternalAccountID,
		Scopes:               payload.Scopes,
		AccessTokenExpiresAt: payload.AccessTokenExpiresAt,
		State:                DelegationState(payload.State),
		UpdatedAt:            payload.UpdatedAt,
		Version:              secret.ResourceVersion,
	}, nil
}

func (s *SecretDelegationStore) Put(ctx context.Context, record *Delegation) error {
	if err := validateDelegationKey(record.Key); err != nil {
		return err
	}
	payload := delegationPayload{
		AccessToken:          record.AccessToken,
		RefreshToken:         record.RefreshToken,
		ExternalAccountID:    record.ExternalAccountID,
		Scopes:               record.Scopes,
		AccessTokenExpiresAt: record.AccessTokenExpiresAt,
		State:                string(record.State),
		UpdatedAt:            record.UpdatedAt,
	}
	sealed, err := s.seal(payload, delegationContext(record.Key)...)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DelegationSecretName(record.Key),
			Namespace: s.namespace,
			Labels: map[string]string{
				LabelRecordKind:         "user-delegation",
				LabelAgentIdentity:      record.Key.AgentIdentity,
				LabelCredentialProvider: record.Key.CredentialProvider,
			},
		},
		Type: DelegationSecretType,
		Data: map[string][]byte{payloadKey: sealed},
	}

	if record.Version == "" {
		// A create, which fails if something is already there. A refresh that
		// forgot to carry its version must not silently clobber a delegation
		// another writer established in the meantime.
		if err := s.client.Create(ctx, secret); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ErrConflict
			}
			return fmt.Errorf("delegation: create delegation: %w", err)
		}
		record.Version = secret.ResourceVersion
		return nil
	}

	secret.ResourceVersion = record.Version
	if err := s.client.Update(ctx, secret); err != nil {
		if apierrors.IsConflict(err) {
			return ErrConflict
		}
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("delegation: update delegation: %w", err)
	}
	record.Version = secret.ResourceVersion
	return nil
}

func (s *SecretDelegationStore) Delete(ctx context.Context, key DelegationKey) error {
	if err := validateDelegationKey(key); err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: DelegationSecretName(key), Namespace: s.namespace},
	}
	if err := client.IgnoreNotFound(s.client.Delete(ctx, secret)); err != nil {
		return fmt.Errorf("delegation: delete delegation: %w", err)
	}
	return nil
}

func (s *secretStore) seal(payload any, context ...string) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Deliberately not wrapped with the payload: an encoding error message
		// carrying a refresh token would defeat the point of sealing it.
		return nil, fmt.Errorf("delegation: encode record")
	}
	sealed, err := s.keyring.Seal(encoded, context...)
	if err != nil {
		return nil, fmt.Errorf("delegation: seal record: %w", err)
	}
	return sealed, nil
}

func (s *secretStore) open(sealed []byte, into any, context ...string) error {
	if len(sealed) == 0 {
		return fmt.Errorf("delegation: record has no %q", payloadKey)
	}
	plaintext, err := s.keyring.Open(sealed, context...)
	if err != nil {
		// aead already refuses to say whether the key, the ciphertext or the
		// address was wrong, and that is preserved here.
		return fmt.Errorf("delegation: open record: %w", err)
	}
	if err := json.Unmarshal(plaintext, into); err != nil {
		return fmt.Errorf("delegation: decode record")
	}
	return nil
}

func validateSessionKey(key SessionKey) error {
	if key.Namespace == "" {
		return fmt.Errorf("delegation: session namespace must not be empty")
	}
	if key.StateDigest == "" {
		// An empty digest would name the same Secret for every session in a
		// namespace, so the first callback would find somebody else's.
		return fmt.Errorf("delegation: session state digest must not be empty")
	}
	return nil
}

func validateDelegationKey(key DelegationKey) error {
	switch {
	case key.Namespace == "":
		return fmt.Errorf("delegation: namespace must not be empty")
	case key.UserIssuer == "":
		return fmt.Errorf("delegation: user issuer must not be empty")
	case key.UserSubject == "":
		return fmt.Errorf("delegation: user subject must not be empty")
	case key.AgentIdentity == "":
		return fmt.Errorf("delegation: agent identity must not be empty")
	case key.CredentialProvider == "":
		return fmt.Errorf("delegation: credential provider must not be empty")
	}
	return nil
}
