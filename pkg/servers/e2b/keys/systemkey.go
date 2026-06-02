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

package keys

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SystemAuth is a per-route scope tag for the system credential. A route opts
// into system-key access by registering AllowSystemKey(scopes...).
type SystemAuth string

const (
	// SystemAuthConnect grants the system credential the right to invoke
	// /sandboxes/{id}/connect cross-owner. It is the only scope defined in v1.
	SystemAuthConnect SystemAuth = "connect"
)

// ConnectSystemKeySecretName is the dedicated Secret holding the connect-scoped
// system credential. It must be pre-created; the manager never creates it.
const ConnectSystemKeySecretName = "e2b-connect-system-key-store"

// SystemKeyDataKey is the data key inside each system-key Secret holding the
// plaintext credential value.
const SystemKeyDataKey = "key"

const (
	systemKeyByteLen       = 32
	systemKeyGeneratedSize = systemKeyByteLen * 2

	// systemKeyRetryInterval / systemKeyRetryTimeout are the production retry
	// policy for ensuring a Secret is populated. Tests override the per-store
	// fields with millisecond values to exercise the cap without real waiting.
	systemKeyRetryInterval = 1 * time.Second
	systemKeyRetryTimeout  = 30 * time.Second
)

// SystemKeyDef is one entry in the code-defined system-key catalog. It binds a
// logical key (name + stable principal UUID) to its dedicated Secret, its
// granted scopes, and whether it bypasses the sandbox owner-equality check.
type SystemKeyDef struct {
	Name       string       // logical id; also the synthesized principal name
	ID         uuid.UUID    // stable principal id for audit
	SecretName string       // one Secret per key
	Scopes     []SystemAuth // granted scopes
	CrossOwner bool         // bypass the sandbox owner-equality check
}

const (
	SystemKeyNameConnect = "sk-connect"
)

var (
	SystemKeyIDConnect = uuid.MustParse("00000000-0000-0000-0000-000000000001")
)

// systemKeyCatalog is the single source of truth for which key grants which
// scopes. Adding a key is a catalog entry plus manifest/RBAC; no auth-core
// change is required. It is a package var (not a const) so tests can inject a
// custom catalog to exercise validation.
var systemKeyCatalog = []SystemKeyDef{
	{
		Name:       SystemKeyNameConnect,
		ID:         SystemKeyIDConnect,
		SecretName: ConnectSystemKeySecretName,
		Scopes:     []SystemAuth{SystemAuthConnect},
		CrossOwner: true,
	},
}

// SystemKeyStore ensures every catalog Secret is populated, loads each plaintext
// value, and serves O(1) lookups by value. System keys are static (no
// rotation), so the lookup map is built once at startup and read-only after.
type SystemKeyStore struct {
	Namespace string
	Client    client.Client
	APIReader client.Reader

	retryInterval time.Duration
	retryTimeout  time.Duration

	mu      sync.RWMutex
	byValue map[string]*SystemKeyDef // verbatim Secret value -> def
}

func NewSystemKeyStore(c client.Client, r client.Reader, namespace string) *SystemKeyStore {
	return &SystemKeyStore{
		Namespace:     namespace,
		Client:        c,
		APIReader:     r,
		retryInterval: systemKeyRetryInterval,
		retryTimeout:  systemKeyRetryTimeout,
	}
}

// Lookup reports whether the presented value is a system key, returning its def.
// It is a plain map read: system keys are 256-bit random values and Go maps use
// a randomized hash seed, so the residual timing side-channel is negligible.
func (s *SystemKeyStore) Lookup(presented string) (*SystemKeyDef, bool) {
	if presented == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	def, ok := s.byValue[presented]
	return def, ok
}

// EnsureKeys validates the catalog, ensures each Secret, and builds the lookup
// map in one atomic swap. Any violation aborts manager startup.
func (s *SystemKeyStore) EnsureKeys(ctx context.Context) error {
	if err := validateCatalog(); err != nil {
		return err
	}
	m := make(map[string]*SystemKeyDef, len(systemKeyCatalog))
	for i := range systemKeyCatalog {
		def := &systemKeyCatalog[i]
		value, err := s.ensureOne(ctx, def)
		if err != nil {
			return err
		}
		if existing, dup := m[value]; dup {
			return fmt.Errorf("system key %q and %q resolved to the same Secret value; values must be unique", existing.Name, def.Name)
		}
		m[value] = def
	}
	s.mu.Lock()
	s.byValue = m
	s.mu.Unlock()
	return nil
}

// validateCatalog enforces the static-catalog invariants before any I/O:
// unique Name/ID/SecretName, and CrossOwner=true (non-cross-owner system keys
// are not modeled in v1).
func validateCatalog() error {
	names := make(map[string]struct{}, len(systemKeyCatalog))
	ids := make(map[uuid.UUID]struct{}, len(systemKeyCatalog))
	secrets := make(map[string]struct{}, len(systemKeyCatalog))
	for i := range systemKeyCatalog {
		def := &systemKeyCatalog[i]
		if !def.CrossOwner {
			return fmt.Errorf("system key %q has CrossOwner=false; non-cross-owner system keys are not supported", def.Name)
		}
		if _, dup := names[def.Name]; dup {
			return fmt.Errorf("duplicate system key name %q in catalog", def.Name)
		}
		if _, dup := ids[def.ID]; dup {
			return fmt.Errorf("duplicate system key id %q in catalog", def.ID)
		}
		if _, dup := secrets[def.SecretName]; dup {
			return fmt.Errorf("duplicate system key secret %q in catalog", def.SecretName)
		}
		names[def.Name] = struct{}{}
		ids[def.ID] = struct{}{}
		secrets[def.SecretName] = struct{}{}
	}
	return nil
}

// ensureOne resolves a single catalog entry's Secret value, retrying at a fixed
// interval up to the retry timeout. Missing Secret / RBAC errors are
// fail-closed: it never calls Create.
func (s *SystemKeyStore) ensureOne(ctx context.Context, def *SystemKeyDef) (string, error) {
	log := klog.FromContext(ctx).WithValues("secret", def.SecretName, "key", def.Name, "namespace", s.Namespace)
	deadline := time.Now().Add(s.retryTimeout)
	var lastErr error
	for {
		value, done, err := s.tryEnsureSecret(ctx, def)
		if done {
			return value, nil
		}
		lastErr = err
		if err != nil {
			log.Error(err, "system-key Secret not ready; will retry")
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("ensure system key %q within %s: %w", def.Name, s.retryTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.retryInterval):
		}
	}
}

// tryEnsureSecret performs one get -> (load | generate+update) cycle. done=true
// means the value is ready. The value is stored verbatim; TrimSpace is used only
// to decide whether the Secret is blank. Get uses APIReader (cache bypass);
// Update uses Client. A concurrent winner is observed on the next Get, so an
// Update Conflict is just a generic retryable error.
func (s *SystemKeyStore) tryEnsureSecret(ctx context.Context, def *SystemKeyDef) (string, bool, error) {
	secret := &corev1.Secret{}
	if err := s.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: def.SecretName}, secret); err != nil {
		return "", false, fmt.Errorf("get secret %q: %w", def.SecretName, err)
	}
	raw := string(secret.Data[SystemKeyDataKey])
	if strings.TrimSpace(raw) != "" {
		return raw, true, nil // already populated -> load VERBATIM
	}
	generated, err := generateSystemKey()
	if err != nil {
		return "", false, fmt.Errorf("generate system key %q: %w", def.Name, err)
	}
	if len(generated) != systemKeyGeneratedSize {
		return "", false, fmt.Errorf("generated system key %q has unexpected length %d", def.Name, len(generated))
	}
	// Prefix the random segment with the def's stable ID so values produced
	// for distinct catalog entries can never collide by construction, even
	// under multi-replica concurrent startup. Existing populated Secrets
	// are loaded verbatim above and remain untouched.
	generated = def.ID.String() + "." + generated
	cp := secret.DeepCopy()
	if cp.Data == nil {
		cp.Data = map[string][]byte{}
	}
	cp.Data[SystemKeyDataKey] = []byte(generated)
	if err := s.Client.Update(ctx, cp); err != nil {
		return "", false, fmt.Errorf("update secret %q: %w", def.SecretName, err)
	}
	return generated, true, nil
}

func generateSystemKey() (string, error) {
	buf := make([]byte, systemKeyByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
