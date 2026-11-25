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

package peersecurity

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/utils"
)

const (
	// defaultPeerKeyDataKey is the fixed peer-key Secret data key.
	defaultPeerKeyDataKey = "key"

	// serverName is the existing runtime TLS name reused as SNI and server
	// verification name. It is not a new peer DNS name.
	serverName = "agentruntime.sandbox.agents.kruise.io"

	// LoadTimeout bounds peer Secret reads. Callers should derive a cancelable
	// child context from startup cancellation.
	LoadTimeout = 30 * time.Second

	// secretKeySize is the only accepted memberlist key length.
	secretKeySize = 32
)

// Inputs is the parsed peer security startup configuration; a Secret reference with
// empty Namespace and Name disables that item. Gossip encryption and peer HTTP TLS are
// enabled independently; the two TLS references must be set together or both left empty.
// Load reads Secrets only for configured references; with both TLS references empty,
// peer HTTP stays plaintext.
type Inputs struct {
	PeerKeySecret types.NamespacedName
	// TLSServerSecret provides inbound TLS credentials and the CA that verifies peer clients.
	TLSServerSecret types.NamespacedName
	// TLSClientSecret provides this process's outbound TLS credentials and the CA that verifies peer servers.
	TLSClientSecret types.NamespacedName
	// AllowedClientCNs is the raw comma-separated inbound client-identity
	// allow-list. Empty leaves authorization at the trusted CA. A non-empty
	// value requires both TLS secret references.
	AllowedClientCNs string
}

// ParseInputs parses the three namespace/name Secret references, keeps the raw inbound
// identity allow-list, and calls Validate. It reads no Secrets and does not validate
// credential material; Load owns material validation.
func ParseInputs(peerKeySecret, tlsServerSecret, tlsClientSecret, allowedClientCNs string) (Inputs, error) {
	keyRef, err := utils.ParseSecretRef(peerKeySecret)
	if err != nil {
		return Inputs{}, fmt.Errorf("peer key secret: %w", err)
	}
	serverRef, err := utils.ParseSecretRef(tlsServerSecret)
	if err != nil {
		return Inputs{}, fmt.Errorf("peer TLS server secret: %w", err)
	}
	clientRef, err := utils.ParseSecretRef(tlsClientSecret)
	if err != nil {
		return Inputs{}, fmt.Errorf("peer TLS client secret: %w", err)
	}
	in := Inputs{
		PeerKeySecret:    keyRef,
		TLSServerSecret:  serverRef,
		TLSClientSecret:  clientRef,
		AllowedClientCNs: allowedClientCNs,
	}
	if err := in.Validate(); err != nil {
		return Inputs{}, err
	}
	return in, nil
}

// Validate reports local input errors that must fail startup before Secret reads.
func (in Inputs) Validate() error {
	if (in.TLSServerSecret.Name == "") != (in.TLSClientSecret.Name == "") {
		return fmt.Errorf("peer TLS requires both server and client secret references, or neither")
	}
	if in.AllowedClientCNs != "" && in.TLSServerSecret.Name == "" {
		return fmt.Errorf("peer allowed client CNs require peer TLS")
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
	return in.PeerKeySecret.Name != "" || in.TLSServerSecret.Name != "" || in.TLSClientSecret.Name != "" || in.AllowedClientCNs != ""
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
