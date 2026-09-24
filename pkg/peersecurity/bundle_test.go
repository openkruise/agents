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
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTLSBundleLoaders(t *testing.T) {
	ca, caKey, caPEM := mustCA(t)
	clientCert, clientKey := mustIssued(t, ca, caKey, x509.ExtKeyUsageClientAuth, nil)
	cmCert, cmKey := mustIssued(t, ca, caKey, x509.ExtKeyUsageClientAuth, nil)
	tests := []struct {
		name     string
		data     map[string][]byte
		wantErr  bool
		wantCert []byte
	}{
		{
			name: "ca only yields server-authenticated material",
			data: map[string][]byte{"ca.crt": caPEM},
		},
		{
			name:     "full set yields mutual TLS material",
			data:     map[string][]byte{"ca.crt": caPEM, "client.crt": clientCert, "client.key": clientKey},
			wantCert: clientCert,
		},
		{
			name:     "cert-manager keys yield mutual TLS material",
			data:     map[string][]byte{"ca.crt": caPEM, "tls.crt": cmCert, "tls.key": cmKey},
			wantCert: cmCert,
		},
		{
			name: "cert-manager keys take precedence over legacy",
			data: map[string][]byte{
				"ca.crt": caPEM, "tls.crt": cmCert, "tls.key": cmKey,
				"client.crt": clientCert, "client.key": clientKey,
			},
			wantCert: cmCert,
		},
		{
			name:    "client cert without key is an error",
			data:    map[string][]byte{"ca.crt": caPEM, "client.crt": clientCert},
			wantErr: true,
		},
		{
			name:    "missing ca key is an error",
			data:    map[string][]byte{"client.crt": clientCert, "client.key": clientKey},
			wantErr: true,
		},
		{
			name:    "unparsable ca is an error",
			data:    map[string][]byte{"ca.crt": []byte("not a pem")},
			wantErr: true,
		},
		{
			name:    "preferred half pair does not fall back",
			data:    map[string][]byte{"ca.crt": caPEM, "tls.crt": cmCert, "client.crt": clientCert, "client.key": clientKey},
			wantErr: true,
		},
		{
			name:    "mixed names never form a pair",
			data:    map[string][]byte{"ca.crt": caPEM, "tls.crt": cmCert, "client.key": cmKey},
			wantErr: true,
		},
		{
			name:    "invalid preferred pair does not fall back",
			data:    map[string][]byte{"ca.crt": caPEM, "tls.crt": []byte("invalid-cert"), "tls.key": []byte("invalid-key"), "client.crt": clientCert, "client.key": clientKey},
			wantErr: true,
		},
		{
			name:    "mismatched preferred pair does not fall back",
			data:    map[string][]byte{"ca.crt": caPEM, "tls.crt": cmCert, "tls.key": clientKey, "client.crt": clientCert, "client.key": clientKey},
			wantErr: true,
		},
	}
	for _, source := range []struct {
		name string
		load func(*testing.T, map[string][]byte) (*TLSBundle, error)
	}{
		{name: "directory", load: func(t *testing.T, data map[string][]byte) (*TLSBundle, error) {
			return NewTLSBundleFromDir(writeBundleDir(t, data))
		}},
		{name: "secret", load: func(t *testing.T, data map[string][]byte) (*TLSBundle, error) {
			reader := fake.NewClientBuilder().WithObjects(secret("certs", "runtime", data)).Build()
			return NewTLSBundleFromSecret(t.Context(), reader, "certs", "runtime")
		}},
	} {
		t.Run(source.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					m, err := source.load(t, tt.data)
					if tt.wantErr {
						require.Error(t, err)
						assert.NotContains(t, err.Error(), "BEGIN")
						assert.Nil(t, m)
						return
					}
					require.NoError(t, err)
					require.NotNil(t, m)
					assert.Equal(t, caPEM, m.CABundle)
					// 加载时必须立即解析并缓存，以便尽早报告错误。
					assert.NotNil(t, m.parsed)
					assert.Equal(t, tt.wantCert, m.ClientCertPEM)
					if tt.wantCert != nil {
						assert.NotEmpty(t, m.ClientKeyPEM)
					} else {
						assert.Empty(t, m.ClientKeyPEM)
					}
				})
			}
		})
	}
}

// 目录入口保留禁用、读取失败和空文件的特有语义。
func TestNewTLSBundleFromDir(t *testing.T) {
	ca, caKey, caPEM := mustCA(t)
	clientCert, clientKey := mustIssued(t, ca, caKey, x509.ExtKeyUsageClientAuth, nil)
	for _, tt := range []struct {
		name    string
		dir     func(*testing.T) string
		wantNil bool
		wantErr bool
	}{
		{
			name:    "empty dir disables TLS",
			dir:     func(*testing.T) string { return "" },
			wantNil: true,
		},
		{
			name:    "missing directory is an error",
			dir:     func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") },
			wantErr: true,
		},
		{
			name: "empty preferred files retain CA-only behavior",
			dir: func(t *testing.T) string {
				return writeBundleDir(t, map[string][]byte{"ca.crt": caPEM, "tls.crt": {}, "tls.key": {}, "client.crt": clientCert, "client.key": clientKey})
			},
		},
		{
			name: "preferred file read error does not fall back",
			dir: func(t *testing.T) string {
				dir := writeBundleDir(t, map[string][]byte{"ca.crt": caPEM, "tls.key": clientKey, "client.crt": clientCert, "client.key": clientKey})
				require.NoError(t, os.Mkdir(filepath.Join(dir, "tls.crt"), 0o700))
				return dir
			},
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewTLSBundleFromDir(tt.dir(t))
			if tt.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "BEGIN")
				assert.Nil(t, m)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, m)
				return
			}
			require.NotNil(t, m)
			assert.Equal(t, caPEM, m.CABundle)
			assert.NotNil(t, m.parsed)
			assert.Empty(t, m.ClientCertPEM)
			assert.Empty(t, m.ClientKeyPEM)
		})
	}
}

// Secret 入口将空值视为缺失，与目录中的空文件不同。
func TestNewTLSBundleFromSecret(t *testing.T) {
	ca, caKey, caPEM := mustCA(t)
	clientCert, clientKey := mustIssued(t, ca, caKey, x509.ExtKeyUsageClientAuth, nil)
	for _, tt := range []struct {
		name       string
		reader     ctrlclient.Reader
		secretName string
		wantNil    bool
		wantErr    bool
	}{
		{
			name:    "empty name disables TLS",
			reader:  fake.NewClientBuilder().Build(),
			wantNil: true,
		},
		{
			name:       "missing secret is an error",
			reader:     fake.NewClientBuilder().Build(),
			secretName: "runtime",
			wantErr:    true,
		},
		{
			name: "empty preferred data falls back",
			reader: fake.NewClientBuilder().WithObjects(secret("certs", "runtime", map[string][]byte{
				"ca.crt": caPEM, "tls.crt": {}, "tls.key": {}, "client.crt": clientCert, "client.key": clientKey,
			})).Build(),
			secretName: "runtime",
		},
		{
			name:       "nil configured reader is an error",
			secretName: "runtime",
			wantErr:    true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewTLSBundleFromSecret(context.Background(), tt.reader, "certs", tt.secretName)
			if tt.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "BEGIN")
				assert.Nil(t, m)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, m)
				return
			}
			require.NotNil(t, m)
			assert.Equal(t, caPEM, m.CABundle)
			assert.NotNil(t, m.parsed)
			assert.Equal(t, clientCert, m.ClientCertPEM)
			assert.Equal(t, clientKey, m.ClientKeyPEM)
		})
	}
}

func writeBundleDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), content, 0o600))
	}
	return dir
}

func TestTLSBundleParsed(t *testing.T) {
	ca, caKey, caPEM := mustCA(t)
	certPEM, keyPEM := mustIssued(t, ca, caKey, x509.ExtKeyUsageClientAuth, nil)
	loaded, err := tlsBundleFromSecret(secret("ns", "client", map[string][]byte{
		"ca.crt": caPEM, "tls.crt": certPEM, "tls.key": keyPEM,
	}))
	require.NoError(t, err)
	for _, tt := range []struct {
		name   string
		bundle TLSBundle
	}{
		{name: "loaded", bundle: *loaded},
		{name: "direct", bundle: TLSBundle{CABundle: caPEM, ClientCertPEM: certPEM, ClientKeyPEM: keyPEM}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.bundle.parsed
			pool, cert, err := tt.bundle.Parsed()
			require.NoError(t, err)
			require.NotNil(t, pool)
			require.NotNil(t, cert)
			assert.True(t, loaded.parsed.rootCAs.Equal(pool))
			assert.Equal(t, loaded.parsed.clientCert.Certificate, cert.Certificate)
			if before != nil {
				assert.Same(t, before.rootCAs, pool)
				assert.Same(t, before.clientCert, cert)
			}
		})
	}
}
