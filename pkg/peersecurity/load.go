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
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Load validates inputs and reads the configured peer
// Secrets through a live uncached reader. Empty Inputs reads nothing and
// returns plaintext materials. TLS ClientAuth on the returned server config is
// left unset so each process can apply its receive policy. A non-empty
// AllowedClientCNs installs VerifyConnection on the server config so inbound
// identity authorization also covers resumed sessions.
func Load(ctx context.Context, reader ctrlclient.Reader, inputs Inputs) (secretKey []byte, serverTLS, clientTLS *tls.Config, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if err := inputs.Validate(); err != nil {
		return nil, nil, nil, err
	}
	needsKey := inputs.PeerKeySecret.Name != ""
	needsTLS := inputs.TLSServerSecret.Name != ""
	if !needsKey && !needsTLS {
		return nil, nil, nil, nil
	}
	if reader == nil {
		return nil, nil, nil, fmt.Errorf("peer secret reader is not configured")
	}

	secrets := loadedSecrets{reader: reader}
	if needsKey {
		keySecret, err := secrets.get(ctx, inputs.PeerKeySecret)
		if err != nil {
			return nil, nil, nil, err
		}
		secretKey, err = loadPeerKey(keySecret)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if needsTLS {
		serverSecret, err := secrets.get(ctx, inputs.TLSServerSecret)
		if err != nil {
			return nil, nil, nil, err
		}
		clientSecret, err := secrets.get(ctx, inputs.TLSClientSecret)
		if err != nil {
			return nil, nil, nil, err
		}
		serverTLS, clientTLS, err = loadTLS(serverSecret, clientSecret)
		if err != nil {
			return nil, nil, nil, err
		}
		attachClientIdentityAllowlist(serverTLS, inputs.AllowedClientCNs)
	}
	return secretKey, serverTLS, clientTLS, nil
}

type loadedSecrets struct {
	reader ctrlclient.Reader
	cache  map[types.NamespacedName]*corev1.Secret
}

func (s *loadedSecrets) get(ctx context.Context, ref types.NamespacedName) (*corev1.Secret, error) {
	if secret, ok := s.cache[ref]; ok {
		return secret, nil
	}
	secret := &corev1.Secret{}
	if err := s.reader.Get(ctx, ref, secret); err != nil {
		return nil, fmt.Errorf("get peer secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	if s.cache == nil {
		s.cache = map[types.NamespacedName]*corev1.Secret{}
	}
	s.cache[ref] = secret
	return secret, nil
}

// loadPeerKey extracts the peer shared key (memberlist gossip key) from the
// key Secret and returns a copy owned by the caller.
func loadPeerKey(secret *corev1.Secret) ([]byte, error) {
	key, err := requiredSecretData(secret, defaultPeerKeyDataKey)
	if err != nil {
		return nil, err
	}
	if len(key) != secretKeySize {
		return nil, fmt.Errorf("peer key secret %s/%s data %q must be exactly %d bytes, got %d",
			secret.Namespace, secret.Name, defaultPeerKeyDataKey, secretKeySize, len(key))
	}
	return append([]byte(nil), key...), nil
}

// loadTLS builds the verified server and client TLS configs from the peer TLS
// Secrets. The server certificate must not also verify as a client credential.
func loadTLS(serverSecret, clientSecret *corev1.Secret) (serverTLS, clientTLS *tls.Config, err error) {
	inboundTrust, serverCert, err := loadPeerTLSBundle(serverSecret)
	if err != nil {
		return nil, nil, fmt.Errorf("load peer TLS server bundle: %w", err)
	}
	outboundTrust, clientCert, err := loadPeerTLSBundle(clientSecret)
	if err != nil {
		return nil, nil, fmt.Errorf("load peer TLS client bundle: %w", err)
	}

	serverLeaf, err := leafOf(serverCert)
	if err != nil {
		return nil, nil, fmt.Errorf("parse peer TLS server certificate: %w", err)
	}
	clientLeaf, err := leafOf(clientCert)
	if err != nil {
		return nil, nil, fmt.Errorf("parse peer TLS client certificate: %w", err)
	}
	now := time.Now()
	serverIntermediates, err := intermediatesOf(serverCert)
	if err != nil {
		return nil, nil, fmt.Errorf("parse peer TLS server intermediates: %w", err)
	}
	clientIntermediates, err := intermediatesOf(clientCert)
	if err != nil {
		return nil, nil, fmt.Errorf("parse peer TLS client intermediates: %w", err)
	}
	// The server certificate must pass ServerAuth verification, as peer clients
	// will apply it.
	if err := verifyCertificate(serverLeaf, outboundTrust, serverIntermediates, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, serverName, now); err != nil {
		return nil, nil, fmt.Errorf("peer TLS server certificate %s failed ServerAuth verification: %w", describeCert(serverLeaf), err)
	}
	// The client certificate must pass ClientAuth verification, as peer servers
	// will apply it.
	if err := verifyCertificate(clientLeaf, inboundTrust, clientIntermediates, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, "", now); err != nil {
		return nil, nil, fmt.Errorf("peer TLS client certificate %s failed ClientAuth verification: %w", describeCert(clientLeaf), err)
	}
	// The server certificate's own EKU must exclude ClientAuth and Any, including
	// when inbound and outbound CAs differ. ClientAuth verification against the
	// inbound trust set can fail for an untrusted issuer; that must not skip
	// this check. An empty EKU is valid for all usages.
	if serverCertAllowsClientAuth(serverLeaf.ExtKeyUsage) {
		return nil, nil, fmt.Errorf("peer TLS server certificate %s allows ClientAuth usage", describeCert(serverLeaf))
	}
	// The server certificate must also fail ClientAuth verification against the
	// inbound trust set; server and client identities must stay distinct.
	if err := verifyCertificate(serverLeaf, inboundTrust, serverIntermediates, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, "", now); err == nil {
		return nil, nil, fmt.Errorf("peer TLS server certificate %s is accepted as a client credential", describeCert(serverLeaf))
	}

	return &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientCAs:    inboundTrust,
			MinVersion:   tls.VersionTLS12,
		}, &tls.Config{
			Certificates: []tls.Certificate{clientCert},
			RootCAs:      outboundTrust,
			ServerName:   serverName,
			MinVersion:   tls.VersionTLS12,
		}, nil
}

func requiredSecretData(secret *corev1.Secret, key string) ([]byte, error) {
	value := secret.Data[key]
	if len(value) == 0 {
		return nil, fmt.Errorf("secret %s/%s data %q is missing or empty", secret.Namespace, secret.Name, key)
	}
	return value, nil
}

// loadPeerTLSBundle adds the peer requirement for a certificate to the shared
// loader's CA-only support. Certificate-purpose checks remain in loadTLS.
func loadPeerTLSBundle(secret *corev1.Secret) (*x509.CertPool, tls.Certificate, error) {
	bundle, err := tlsBundleFromSecret(secret)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	trust, cert, err := bundle.Parsed()
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	if cert == nil {
		return nil, tls.Certificate{}, fmt.Errorf("peer TLS secret %s/%s certificate/key pair is required", secret.Namespace, secret.Name)
	}
	return trust, *cert, nil
}

// leafOf returns the leaf certificate at the head of the certificate chain.
func leafOf(cert tls.Certificate) (*x509.Certificate, error) {
	if cert.Leaf != nil {
		return cert.Leaf, nil
	}
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("certificate chain is empty")
	}
	return x509.ParseCertificate(cert.Certificate[0])
}

// intermediatesOf returns the certificates after the leaf as an intermediate
// pool, or nil when the chain has no intermediates.
func intermediatesOf(cert tls.Certificate) (*x509.CertPool, error) {
	if len(cert.Certificate) < 2 {
		return nil, nil
	}
	pool := x509.NewCertPool()
	for _, raw := range cert.Certificate[1:] {
		parsed, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, err
		}
		pool.AddCert(parsed)
	}
	return pool, nil
}

func serverCertAllowsClientAuth(usages []x509.ExtKeyUsage) bool {
	if len(usages) == 0 {
		return true
	}
	for _, usage := range usages {
		if usage == x509.ExtKeyUsageAny || usage == x509.ExtKeyUsageClientAuth {
			return true
		}
	}
	return false
}

func verifyCertificate(leaf *x509.Certificate, roots, intermediates *x509.CertPool, usages []x509.ExtKeyUsage, dnsName string, now time.Time) error {
	opts := x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     usages,
		DNSName:       dnsName,
	}
	_, err := leaf.Verify(opts)
	return err
}

func describeCert(cert *x509.Certificate) string {
	return fmt.Sprintf("subject=%q issuer=%q serial=%s notBefore=%s notAfter=%s",
		cert.Subject, cert.Issuer, cert.SerialNumber.String(),
		cert.NotBefore.UTC().Format(time.RFC3339), cert.NotAfter.UTC().Format(time.RFC3339))
}
