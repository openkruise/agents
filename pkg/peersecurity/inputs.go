/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

    10|Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package peersecurity

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

const (
	// defaultPeerKeyDataKey is the peer-key Secret data key when unset.
	defaultPeerKeyDataKey = "key"
	// defaultCAKey is the CA bundle data key when unset.
	defaultCAKey = "ca.crt"
	// defaultTLSCertKey is the Kubernetes TLS certificate data key.
	defaultTLSCertKey = "tls.crt"
	// defaultTLSKeyKey is the Kubernetes TLS private-key data key.
	defaultTLSKeyKey = "tls.key"
	// defaultClientCertKey is the default client certificate data key, matching
	// the runtime client bundle naming.
	defaultClientCertKey = "client.crt"
	// defaultClientKeyKey is the default client private-key data key, matching
	// the runtime client bundle naming.
	defaultClientKeyKey = "client.key"

	// serverName is the existing runtime TLS name reused as SNI and server
	// verification name. It is not a new peer DNS name.
	serverName = "agentruntime.sandbox.agents.kruise.io"

	// LoadTimeout bounds peer Secret reads. Callers should derive a cancelable
	// child context from startup cancellation.
	LoadTimeout = 30 * time.Second

	// secretKeySize is the only accepted memberlist key length.
	secretKeySize = 32
)

// Inputs is the parsed peer-security startup configuration. A zero NamespacedName
// (empty Name) disables that Secret. Data-key fields keep empty values until
// ApplyDefaults fills them; empty after that still means "use the default".
type Inputs struct {
	PeerKeySecret     types.NamespacedName
	PeerKeyDataKey    string
	TLSServerSecret   types.NamespacedName
	TLSClientSecret   types.NamespacedName
	ServerCADataKey   string
	ServerCertDataKey string
	ServerKeyDataKey  string
	ClientCADataKey   string
	ClientCertDataKey string
	ClientKeyDataKey  string
}

// ApplyDefaults fills empty data-key fields. The client certificate and key
// default to the runtime client bundle names; a component whose bundle uses
// other names configures those two keys explicitly.
func (in *Inputs) ApplyDefaults() {
	in.PeerKeyDataKey = defaultDataKey(in.PeerKeyDataKey, defaultPeerKeyDataKey)
	in.ServerCADataKey = defaultDataKey(in.ServerCADataKey, defaultCAKey)
	in.ServerCertDataKey = defaultDataKey(in.ServerCertDataKey, defaultTLSCertKey)
	in.ServerKeyDataKey = defaultDataKey(in.ServerKeyDataKey, defaultTLSKeyKey)
	in.ClientCADataKey = defaultDataKey(in.ClientCADataKey, defaultCAKey)
	in.ClientCertDataKey = defaultDataKey(in.ClientCertDataKey, defaultClientCertKey)
	in.ClientKeyDataKey = defaultDataKey(in.ClientKeyDataKey, defaultClientKeyKey)
}

func defaultDataKey(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// Validate reports local input errors that must fail startup before Secret reads.
func (in Inputs) Validate() error {
	if (in.TLSServerSecret.Name == "") != (in.TLSClientSecret.Name == "") {
		return fmt.Errorf("peer TLS requires both server and client secret references, or neither")
	}
	if err := requireCompleteRef("peer key secret", in.PeerKeySecret); err != nil {
		return err
	}
	if err := requireCompleteRef("peer TLS server secret", in.TLSServerSecret); err != nil {
		return err
	}
	return requireCompleteRef("peer TLS client secret", in.TLSClientSecret)
}

func (in Inputs) Configured() bool {
	return in.PeerKeySecret.Name != "" || in.TLSServerSecret.Name != "" || in.TLSClientSecret.Name != ""
}

func requireCompleteRef(what string, ref types.NamespacedName) error {
	if ref.Name == "" && ref.Namespace == "" {
		return nil
	}
	if ref.Name == "" || ref.Namespace == "" {
		return fmt.Errorf("%s reference is incomplete", what)
	}
	return nil
}
