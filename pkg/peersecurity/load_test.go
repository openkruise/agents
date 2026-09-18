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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/pkg/utils"
)

func TestLoadPlaintext(t *testing.T) {
	key, serverTLS, clientTLS, err := Load(t.Context(), nil, Inputs{})
	require.NoError(t, err)
	assert.Nil(t, key)
	assert.Nil(t, serverTLS)
	assert.Nil(t, clientTLS)
}

func TestLoadCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, _, err := Load(ctx, fake.NewClientBuilder().Build(), Inputs{
		PeerKeySecret: types.NamespacedName{Namespace: "ns", Name: "key"},
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestLoadNilReader(t *testing.T) {
	_, _, _, err := Load(t.Context(), nil, Inputs{
		PeerKeySecret: types.NamespacedName{Namespace: "ns", Name: "key"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "peer secret reader is not configured")
}

func TestLoadInvalidInputs(t *testing.T) {
	_, _, _, err := Load(t.Context(), fake.NewClientBuilder().Build(), Inputs{
		TLSServerSecret: types.NamespacedName{Namespace: "ns", Name: "server"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both server and client")
}

func TestLoadPeerKey(t *testing.T) {
	valid := bytes32('k')
	tests := []struct {
		name    string
		data    map[string][]byte
		dataKey string
		wantErr string
		wantKey []byte
	}{
		{name: "32 bytes", data: map[string][]byte{"key": valid}, wantKey: valid},
		{name: "custom key name", data: map[string][]byte{"token": valid}, dataKey: "token", wantKey: valid},
		{name: "16 bytes rejected", data: map[string][]byte{"key": make([]byte, 16)}, wantErr: "exactly 32 bytes"},
		{name: "24 bytes rejected", data: map[string][]byte{"key": make([]byte, 24)}, wantErr: "exactly 32 bytes"},
		{name: "missing key", data: map[string][]byte{}, wantErr: "missing or empty"},
		{name: "wrong key name not guessed", data: map[string][]byte{"token": valid}, wantErr: "missing or empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataKey := tt.dataKey
			if dataKey == "" {
				dataKey = defaultPeerKeyDataKey
			}
			key, err := loadPeerKey(secret("ns", "peer-key", tt.data), dataKey)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKey, key)
		})
	}
}

func TestLoadTLS(t *testing.T) {
	caCert, caKey, caPEM := mustCA(t)
	serverCert, serverKey := mustIssued(t, caCert, caKey, x509.ExtKeyUsageServerAuth, []string{serverName})
	clientCert, clientKey := mustIssued(t, caCert, caKey, x509.ExtKeyUsageClientAuth, nil)
	bothCert, bothKey := mustIssued(t, caCert, caKey, x509.ExtKeyUsageServerAuth, []string{serverName}, x509.ExtKeyUsageClientAuth)
	noEKUCert, noEKUKey := mustIssued(t, caCert, caKey, 0, []string{serverName})
	anyCert, anyKey := mustIssued(t, caCert, caKey, 0, []string{serverName}, x509.ExtKeyUsageAny)
	wrongNameCert, wrongNameKey := mustIssued(t, caCert, caKey, x509.ExtKeyUsageServerAuth, []string{"other.example"})

	clientData := map[string][]byte{
		"ca.crt":     caPEM,
		"client.crt": clientCert,
		"client.key": clientKey,
	}

	tests := []struct {
		name       string
		serverData map[string][]byte
		clientData map[string][]byte
		wantErr    string
	}{
		{
			name:       "valid materials",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": serverCert, "tls.key": serverKey},
			clientData: clientData,
		},
		{
			name:       "missing data key",
			serverData: map[string][]byte{"ca.crt": caPEM},
			clientData: clientData,
			wantErr:    "missing or empty",
		},
		{
			name:       "server cert allows ClientAuth",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": bothCert, "tls.key": bothKey},
			clientData: clientData,
			wantErr:    "allows ClientAuth usage",
		},
		{
			name:       "server cert omits EKU",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": noEKUCert, "tls.key": noEKUKey},
			clientData: clientData,
			wantErr:    "allows ClientAuth usage",
		},
		{
			name:       "server cert allows Any usage",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": anyCert, "tls.key": anyKey},
			clientData: clientData,
			wantErr:    "allows ClientAuth usage",
		},
		{
			name:       "server name mismatch",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": wrongNameCert, "tls.key": wrongNameKey},
			clientData: clientData,
			wantErr:    "ServerAuth",
		},
		{
			name:       "invalid server CA PEM",
			serverData: map[string][]byte{"ca.crt": []byte("not-a-pem"), "tls.crt": serverCert, "tls.key": serverKey},
			clientData: clientData,
			wantErr:    "parse peer TLS server CA",
		},
		{
			name:       "invalid client CA PEM",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": serverCert, "tls.key": serverKey},
			clientData: map[string][]byte{"ca.crt": []byte("not-a-pem"), "client.crt": clientCert, "client.key": clientKey},
			wantErr:    "parse peer TLS client CA",
		},
		{
			name:       "missing server key",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": serverCert},
			clientData: clientData,
			wantErr:    "missing or empty",
		},
		{
			name:       "mismatched server key pair",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": serverCert, "tls.key": clientKey},
			clientData: clientData,
			wantErr:    "parse certificate/key pair",
		},
		{
			name:       "missing server CA after key pair",
			serverData: map[string][]byte{"tls.crt": serverCert, "tls.key": serverKey},
			clientData: clientData,
			wantErr:    "missing or empty",
		},
		{
			name:       "missing client certificate",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": serverCert, "tls.key": serverKey},
			clientData: map[string][]byte{"ca.crt": caPEM, "client.key": clientKey},
			wantErr:    "missing or empty",
		},
		{
			name:       "missing client CA after key pair",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": serverCert, "tls.key": serverKey},
			clientData: map[string][]byte{"client.crt": clientCert, "client.key": clientKey},
			wantErr:    "missing or empty",
		},
		{
			name:       "client cert fails ClientAuth",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": serverCert, "tls.key": serverKey},
			clientData: map[string][]byte{"ca.crt": caPEM, "client.crt": serverCert, "client.key": serverKey},
			wantErr:    "ClientAuth",
		},
		{
			name: "invalid server intermediate",
			serverData: map[string][]byte{
				"ca.crt":  caPEM,
				"tls.crt": append(append([]byte{}, serverCert...), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("bad")})...),
				"tls.key": serverKey,
			},
			clientData: clientData,
			wantErr:    "parse peer TLS server intermediates",
		},
		{
			name:       "invalid client intermediate",
			serverData: map[string][]byte{"ca.crt": caPEM, "tls.crt": serverCert, "tls.key": serverKey},
			clientData: map[string][]byte{
				"ca.crt":     caPEM,
				"client.crt": append(append([]byte{}, clientCert...), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("bad")})...),
				"client.key": clientKey,
			},
			wantErr: "parse peer TLS client intermediates",
		},
	}
	inputs := tlsInputs("ns/server", "ns/client")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverTLS, clientTLS, err := loadTLS(
				secret("ns", "server", tt.serverData), secret("ns", "client", tt.clientData), inputs)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.NotContains(t, err.Error(), "BEGIN")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, serverTLS)
			require.NotNil(t, clientTLS)
			assert.Equal(t, serverName, clientTLS.ServerName)
		})
	}
}

func TestLoadTLSSplitCA(t *testing.T) {
	inboundCA, inboundKey, inboundPEM := mustCA(t)
	outboundCA, outboundKey, outboundPEM := mustCA(t)
	serverCert, serverKey := mustIssued(t, outboundCA, outboundKey, x509.ExtKeyUsageServerAuth, []string{serverName})
	clientCert, clientKey := mustIssued(t, inboundCA, inboundKey, x509.ExtKeyUsageClientAuth, nil)
	bothCert, bothKey := mustIssued(t, outboundCA, outboundKey, x509.ExtKeyUsageServerAuth, []string{serverName}, x509.ExtKeyUsageClientAuth)
	noEKUCert, noEKUKey := mustIssued(t, outboundCA, outboundKey, 0, []string{serverName})
	anyCert, anyKey := mustIssued(t, outboundCA, outboundKey, 0, []string{serverName}, x509.ExtKeyUsageAny)

	clientData := map[string][]byte{
		"ca.crt":     outboundPEM,
		"client.crt": clientCert,
		"client.key": clientKey,
	}
	validServer := map[string][]byte{"ca.crt": inboundPEM, "tls.crt": serverCert, "tls.key": serverKey}
	inputs := tlsInputs("ns/server", "ns/client")

	tests := []struct {
		name       string
		serverData map[string][]byte
		wantErr    string
	}{
		{name: "server-auth only", serverData: validServer},
		{
			name:       "server cert allows ClientAuth",
			serverData: map[string][]byte{"ca.crt": inboundPEM, "tls.crt": bothCert, "tls.key": bothKey},
			wantErr:    "allows ClientAuth usage",
		},
		{
			name:       "server cert omits EKU",
			serverData: map[string][]byte{"ca.crt": inboundPEM, "tls.crt": noEKUCert, "tls.key": noEKUKey},
			wantErr:    "allows ClientAuth usage",
		},
		{
			name:       "server cert allows Any usage",
			serverData: map[string][]byte{"ca.crt": inboundPEM, "tls.crt": anyCert, "tls.key": anyKey},
			wantErr:    "allows ClientAuth usage",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverTLS, clientTLS, err := loadTLS(
				secret("ns", "server", tt.serverData), secret("ns", "client", clientData), inputs)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.NotContains(t, err.Error(), "BEGIN")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, serverTLS)
			require.NotNil(t, clientTLS)
			assert.Equal(t, serverName, clientTLS.ServerName)
		})
	}
}

func TestLoadSecretReads(t *testing.T) {
	caCert, caKey, caPEM := mustCA(t)
	serverCert, serverKey := mustIssued(t, caCert, caKey, x509.ExtKeyUsageServerAuth, []string{serverName})
	clientCert, clientKey := mustIssued(t, caCert, caKey, x509.ExtKeyUsageClientAuth, nil)

	serverData := map[string][]byte{
		"ca.crt":  caPEM,
		"tls.crt": serverCert,
		"tls.key": serverKey,
	}
	clientData := map[string][]byte{
		"ca.crt":     caPEM,
		"client.crt": clientCert,
		"client.key": clientKey,
	}
	sharedData := map[string][]byte{
		"ca.crt":     caPEM,
		"tls.crt":    serverCert,
		"tls.key":    serverKey,
		"client.crt": clientCert,
		"client.key": clientKey,
	}
	keyAndTLS := tlsInputs("ns/server", "ns/client")
	keyAndTLS.PeerKeySecret = types.NamespacedName{Namespace: "ns", Name: "peer-key"}

	tests := []struct {
		name    string
		objects []ctrlclient.Object
		inputs  Inputs
		gets    int
		wantErr string
		wantKey []byte
	}{
		{
			name:    "separate secrets",
			objects: []ctrlclient.Object{secret("ns", "server", serverData), secret("ns", "client", clientData)},
			inputs:  tlsInputs("ns/server", "ns/client"),
			gets:    2,
		},
		{
			name: "key and TLS",
			objects: []ctrlclient.Object{
				secret("ns", "peer-key", map[string][]byte{"key": bytes32('x')}),
				secret("ns", "server", serverData), secret("ns", "client", clientData),
			},
			inputs:  keyAndTLS,
			gets:    3,
			wantKey: bytes32('x'),
		},
		{
			name:    "same secret read once",
			objects: []ctrlclient.Object{secret("ns", "bundle", sharedData)},
			inputs:  tlsInputs("ns/bundle", "ns/bundle"),
			gets:    1,
		},
		{
			name:    "missing TLS secret",
			objects: nil,
			inputs:  tlsInputs("ns/server", "ns/client"),
			wantErr: "get peer secret",
		},
		{
			name:    "missing key secret",
			objects: nil,
			inputs:  Inputs{PeerKeySecret: types.NamespacedName{Namespace: "ns", Name: "peer-key"}},
			wantErr: "get peer secret",
		},
		{
			name:    "short peer key",
			objects: []ctrlclient.Object{secret("ns", "peer-key", map[string][]byte{"key": make([]byte, 16)})},
			inputs:  Inputs{PeerKeySecret: types.NamespacedName{Namespace: "ns", Name: "peer-key"}},
			wantErr: "exactly 32 bytes",
		},
		{
			name:    "missing client TLS secret",
			objects: []ctrlclient.Object{secret("ns", "server", serverData)},
			inputs:  tlsInputs("ns/server", "ns/client"),
			wantErr: "get peer secret",
		},
		{
			name:    "TLS materials fail loadTLS",
			objects: []ctrlclient.Object{secret("ns", "server", map[string][]byte{"ca.crt": caPEM}), secret("ns", "client", clientData)},
			inputs:  tlsInputs("ns/server", "ns/client"),
			wantErr: "missing or empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gets atomic.Int32
			base := fake.NewClientBuilder().WithObjects(tt.objects...).Build()
			reader := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
					gets.Add(1)
					return c.Get(ctx, key, obj, opts...)
				},
			})
			key, serverTLS, clientTLS, err := Load(t.Context(), reader, tt.inputs)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKey, key)
			require.NotNil(t, serverTLS)
			require.NotNil(t, clientTLS)
			assert.Equal(t, serverName, clientTLS.ServerName)
			assert.Equal(t, tt.gets, int(gets.Load()))
		})
	}
}

func TestLeafOfAndIntermediates(t *testing.T) {
	ca, caKey, _ := mustCA(t)
	leafPEM, _ := mustIssued(t, ca, caKey, x509.ExtKeyUsageServerAuth, []string{serverName})
	leafBlock, _ := pem.Decode(leafPEM)
	require.NotNil(t, leafBlock)

	t.Run("empty chain", func(t *testing.T) {
		_, err := leafOf(tls.Certificate{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "certificate chain is empty")
	})
	t.Run("invalid leaf DER", func(t *testing.T) {
		_, err := leafOf(tls.Certificate{Certificate: [][]byte{[]byte("bad")}})
		require.Error(t, err)
	})
	t.Run("parses leaf when Leaf is unset", func(t *testing.T) {
		parsed, err := leafOf(tls.Certificate{Certificate: [][]byte{leafBlock.Bytes}})
		require.NoError(t, err)
		assert.Equal(t, leafBlock.Bytes, parsed.Raw)
	})
	t.Run("invalid intermediate DER", func(t *testing.T) {
		_, err := intermediatesOf(tls.Certificate{Certificate: [][]byte{leafBlock.Bytes, []byte("bad")}})
		require.Error(t, err)
	})
	t.Run("intermediate pool", func(t *testing.T) {
		pool, err := intermediatesOf(tls.Certificate{Certificate: [][]byte{leafBlock.Bytes, ca.Raw}})
		require.NoError(t, err)
		require.NotNil(t, pool)
	})
}

func tlsInputs(server, client string) Inputs {
	serverRef, _ := utils.ParseSecretRef(server)
	clientRef, _ := utils.ParseSecretRef(client)
	in := Inputs{TLSServerSecret: serverRef, TLSClientSecret: clientRef}
	in.ApplyDefaults()
	return in
}

func secret(namespace, name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       data,
	}
}

func bytes32(b byte) []byte {
	key := make([]byte, secretKeySize)
	for i := range key {
		key[i] = b
	}
	return key
}

func mustCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "peer-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func mustIssued(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, usage x509.ExtKeyUsage, dnsNames []string, extraUsage ...x509.ExtKeyUsage) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "peer-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		DNSNames:     dnsNames,
	}
	if usage != 0 {
		tmpl.ExtKeyUsage = append([]x509.ExtKeyUsage{usage}, extraUsage...)
	} else if len(extraUsage) > 0 {
		tmpl.ExtKeyUsage = extraUsage
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
