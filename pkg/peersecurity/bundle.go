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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// TLSBundle is a startup snapshot of CA and optional certificate/key material.
// The ClientCertPEM and ClientKeyPEM names are retained for existing runtime
// callers; peer servers also use these fields for their server credentials.
// Callers own certificate-purpose, trust-direction, and authentication policy.
// The bundle, its PEM slices, and its parsed material must be treated as read-only.
type TLSBundle struct {
	// CABundle is the required PEM-encoded trust bundle.
	CABundle []byte
	// ClientCertPEM and ClientKeyPEM must be supplied together, or both omitted.
	ClientCertPEM []byte
	ClientKeyPEM  []byte

	// Loaders decode eagerly to fail at startup and avoid repeated PEM parsing.
	// Directly constructed bundles are decoded on demand without cache writes.
	parsed *parsedTLSBundle
}

type parsedTLSBundle struct {
	rootCAs    *x509.CertPool
	clientCert *tls.Certificate
}

// Parsed returns read-only decoded material, reusing the startup cache when
// available. The certificate is nil for a CA-only bundle. This method does not
// validate certificate chains, lifetimes, names, or usages; consumers do so
// according to their TLS policy. It never mutates the bundle.
func (m TLSBundle) Parsed() (*x509.CertPool, *tls.Certificate, error) {
	parsed := m.parsed
	if parsed == nil {
		var err error
		parsed, err = parseTLSBundle(m)
		if err != nil {
			return nil, nil, err
		}
	}
	return parsed.rootCAs, parsed.clientCert, nil
}

func parseTLSBundle(m TLSBundle) (*parsedTLSBundle, error) {
	if len(m.CABundle) == 0 {
		return nil, fmt.Errorf("TLS CA bundle is required")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(m.CABundle) {
		return nil, fmt.Errorf("failed to parse TLS CA bundle")
	}
	parsed := &parsedTLSBundle{rootCAs: pool}
	if len(m.ClientCertPEM) > 0 || len(m.ClientKeyPEM) > 0 {
		cert, err := tls.X509KeyPair(m.ClientCertPEM, m.ClientKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("parse certificate/key pair: %w", err)
		}
		parsed.clientCert = &cert
	}
	return parsed, nil
}

// NewTLSBundleFromDir loads ca.crt and an optional certificate/key pair from dir.
// tls.crt/tls.key take precedence over client.crt/client.key. Only a pair of
// absent files permits trying the next names; other read or parse errors fail.
// An empty dir disables TLS and returns nil. A configured directory with no
// certificate files yields a CA-only bundle for server-authenticated TLS.
// Material is loaded once; replacing it requires restarting the caller.
func NewTLSBundleFromDir(dir string) (*TLSBundle, error) {
	if dir == "" {
		return nil, nil
	}
	bundle, err := readTLSBundle(func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- operator-configured certificate directory
	})
	if err != nil {
		return nil, fmt.Errorf("load TLS bundle in directory %s: %w", dir, err)
	}
	return bundle, nil
}

// NewTLSBundleFromSecret is the Secret-backed counterpart of NewTLSBundleFromDir.
// An empty name disables TLS. Empty Secret values count as absent, so a pair
// with exactly one non-empty value is an error. CA-only bundles are allowed;
// consumers requiring mutual TLS must additionally require a certificate.
func NewTLSBundleFromSecret(ctx context.Context, reader ctrlclient.Reader, namespace, name string) (*TLSBundle, error) {
	if name == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, fmt.Errorf("TLS secret reader is not configured")
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("get TLS secret %s/%s: %w", namespace, name, err)
	}
	return tlsBundleFromSecret(secret)
}

// tlsBundleFromSecret also accepts Secrets already fetched by the peer loader,
// keeping that loader's per-startup read deduplication intact.
func tlsBundleFromSecret(secret *corev1.Secret) (*TLSBundle, error) {
	bundle, err := readTLSBundle(func(name string) ([]byte, error) {
		data := secret.Data[name]
		if len(data) == 0 {
			return nil, fmt.Errorf("data %q is missing or empty: %w", name, fs.ErrNotExist)
		}
		return data, nil
	})
	if err != nil {
		return nil, fmt.Errorf("load TLS bundle in secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return bundle, nil
}

// readTLSBundle owns name selection and parsing for every source. Readers use
// fs.ErrNotExist to mark absent values. Once either member of a naming pair is
// present, errors are final: never mix names or try another identity on failure.
func readTLSBundle(read func(string) ([]byte, error)) (*TLSBundle, error) {
	ca, err := read("ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read TLS CA bundle: %w", err)
	}
	m := &TLSBundle{CABundle: ca}
	for _, names := range [...]struct{ cert, key string }{
		{"tls.crt", "tls.key"},
		{"client.crt", "client.key"},
	} {
		cert, certErr := read(names.cert)
		key, keyErr := read(names.key)
		switch {
		case errors.Is(certErr, fs.ErrNotExist) && errors.Is(keyErr, fs.ErrNotExist):
			continue
		case certErr != nil:
			return nil, fmt.Errorf("read TLS certificate %q: %w", names.cert, certErr)
		case keyErr != nil:
			return nil, fmt.Errorf("read TLS private key %q: %w", names.key, keyErr)
		}
		m.ClientCertPEM, m.ClientKeyPEM = cert, key
		break
	}
	parsed, err := parseTLSBundle(*m)
	if err != nil {
		return nil, err
	}
	m.parsed = parsed
	return m, nil
}
