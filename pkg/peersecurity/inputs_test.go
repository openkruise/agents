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

func TestApplyDefaults(t *testing.T) {
	in := Inputs{PeerKeyDataKey: "custom"}
	in.ApplyDefaults()
	assert.Equal(t, "custom", in.PeerKeyDataKey)
	assert.Equal(t, defaultCAKey, in.ServerCADataKey)
	assert.Equal(t, defaultTLSCertKey, in.ServerCertDataKey)
	assert.Equal(t, defaultTLSKeyKey, in.ServerKeyDataKey)
	assert.Equal(t, defaultCAKey, in.ClientCADataKey)
	assert.Equal(t, defaultClientCertKey, in.ClientCertDataKey)
	assert.Equal(t, defaultClientKeyKey, in.ClientKeyDataKey)

	// Pre-set client keys are kept.
	preset := Inputs{ClientCertDataKey: "other.crt"}
	preset.ApplyDefaults()
	assert.Equal(t, "other.crt", preset.ClientCertDataKey)
	assert.Equal(t, defaultClientKeyKey, preset.ClientKeyDataKey)
}
