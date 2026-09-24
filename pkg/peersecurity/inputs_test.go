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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestValidateTLSPair(t *testing.T) {
	tests := []struct {
		name    string
		inputs  Inputs
		wantErr string
	}{
		{name: "all empty"},
		{
			name: "both TLS refs",
			inputs: Inputs{
				TLSServerSecret: types.NamespacedName{Namespace: "ns", Name: "server"},
				TLSClientSecret: types.NamespacedName{Namespace: "ns", Name: "client"},
			},
		},
		{
			name: "only server TLS",
			inputs: Inputs{
				TLSServerSecret: types.NamespacedName{Namespace: "ns", Name: "server"},
			},
			wantErr: "both server and client",
		},
		{
			name: "only client TLS",
			inputs: Inputs{
				TLSClientSecret: types.NamespacedName{Namespace: "ns", Name: "client"},
			},
			wantErr: "both server and client",
		},
		{
			name: "incomplete key ref",
			inputs: Inputs{
				PeerKeySecret: types.NamespacedName{Name: "key"},
			},
			wantErr: "incomplete",
		},
		{
			name: "incomplete TLS server ref",
			inputs: Inputs{
				TLSServerSecret: types.NamespacedName{Name: "server"},
				TLSClientSecret: types.NamespacedName{Namespace: "ns", Name: "client"},
			},
			wantErr: "incomplete",
		},
		{
			name: "incomplete TLS client ref",
			inputs: Inputs{
				TLSServerSecret: types.NamespacedName{Namespace: "ns", Name: "server"},
				TLSClientSecret: types.NamespacedName{Name: "client"},
			},
			wantErr: "incomplete",
		},
		{
			name: "allowlist without TLS",
			inputs: Inputs{
				AllowedClientCNs: "sandbox-manager",
			},
			wantErr: "peer allowed client CNs require peer TLS",
		},
		{
			name: "comma-only allowlist without TLS",
			inputs: Inputs{
				AllowedClientCNs: ",",
			},
			wantErr: "peer allowed client CNs require peer TLS",
		},
		{
			name: "allowlist with both TLS refs",
			inputs: Inputs{
				TLSServerSecret:  types.NamespacedName{Namespace: "ns", Name: "server"},
				TLSClientSecret:  types.NamespacedName{Namespace: "ns", Name: "client"},
				AllowedClientCNs: "sandbox-manager",
			},
		},
		{
			name:    "empty allowlist stays plaintext",
			inputs:  Inputs{AllowedClientCNs: ""},
			wantErr: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.inputs.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestConfigured(t *testing.T) {
	tests := []struct {
		name   string
		inputs Inputs
		want   bool
	}{
		{name: "empty"},
		{
			name:   "peer key",
			inputs: Inputs{PeerKeySecret: types.NamespacedName{Namespace: "ns", Name: "key"}},
			want:   true,
		},
		{
			name:   "TLS server",
			inputs: Inputs{TLSServerSecret: types.NamespacedName{Namespace: "ns", Name: "server"}},
			want:   true,
		},
		{
			name:   "TLS client",
			inputs: Inputs{TLSClientSecret: types.NamespacedName{Namespace: "ns", Name: "client"}},
			want:   true,
		},
		{
			name:   "allowlist",
			inputs: Inputs{AllowedClientCNs: "sandbox-manager"},
			want:   true,
		},
		{
			name:   "comma-only allowlist",
			inputs: Inputs{AllowedClientCNs: ","},
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.inputs.Configured())
		})
	}
}
