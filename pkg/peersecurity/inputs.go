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

// Inputs 是解析后的 peer 安全启动配置；Secret 引用的 Namespace 和 Name 均为空时禁用该项。
// gossip 加密与 peer HTTP TLS 独立启用；两个 TLS 引用必须同时配置或同时留空。
// Load 仅在对应引用已配置时读取 Secret；两个 TLS 引用均为空时保留明文 peer HTTP。
type Inputs struct {
	PeerKeySecret types.NamespacedName
	// TLSServerSecret 提供入站 TLS 凭证，以及验证 peer 客户端的 CA。
	TLSServerSecret types.NamespacedName
	// TLSClientSecret 提供本进程的出站 TLS 凭证，以及验证 peer 服务端的 CA。
	TLSClientSecret types.NamespacedName
	// AllowedClientCNs is the raw comma-separated inbound client-identity
	// allow-list. Empty leaves authorization at the trusted CA. A non-empty
	// value requires both TLS secret references.
	AllowedClientCNs string
}

// ParseInputs 解析三个 namespace/name Secret 引用，保留入站身份白名单原文并调用 Validate。
// 此函数不读取 Secret，也不校验凭证材料；材料校验由 Load 负责。
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
