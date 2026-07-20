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

package runtimecredentials

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	DefaultOutputDirectory = "/var/run/sandbox-gateway/runtime-mtls"

	CertificateKey = "tls.crt"
	PrivateKeyKey  = "tls.key"
	CAKey          = "ca.crt"

	CertificateFile = "tls.crt"
	PrivateKeyFile  = "tls.key"
	CAFile          = "ca.crt"

	maxCredentialBytes = 1 << 20
)

const (
	envSecretNamespace = "RUNTIME_MTLS_SECRET_NAMESPACE"
	envSecretName      = "RUNTIME_MTLS_SECRET_NAME"
)

// Options identifies the source Secret and destination directory.
type Options struct {
	SecretNamespace string
	SecretName      string
	OutputDirectory string
}

// OptionsFromEnvironment returns the configured Secret source and fixed output location.
func OptionsFromEnvironment() (Options, error) {
	opts := Options{
		SecretNamespace: os.Getenv(envSecretNamespace),
		SecretName:      os.Getenv(envSecretName),
		OutputDirectory: DefaultOutputDirectory,
	}
	if err := opts.validate(); err != nil {
		return Options{}, fmt.Errorf("load runtime mTLS options: %w", err)
	}
	return opts, nil
}

type credentialFiles struct {
	certificate []byte
	privateKey  []byte
	ca          []byte
}

// Load fetches, validates, and writes the static runtime mTLS credentials.
func Load(ctx context.Context, reader client.Reader, opts Options) error {
	if reader == nil {
		return errors.New("Kubernetes reader must not be nil")
	}
	if err := opts.validate(); err != nil {
		return err
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: opts.SecretNamespace, Name: opts.SecretName}
	err := reader.Get(ctx, key, secret)
	if err != nil {
		return fmt.Errorf("get runtime mTLS Secret %s/%s: %w", opts.SecretNamespace, opts.SecretName, err)
	}
	files, err := credentialsFromSecret(secret.Data, time.Now())
	if err != nil {
		return fmt.Errorf("validate runtime mTLS Secret %s/%s: %w", opts.SecretNamespace, opts.SecretName, err)
	}
	if err := writeCredentialFiles(opts.OutputDirectory, files); err != nil {
		return fmt.Errorf("write runtime mTLS credentials: %w", err)
	}
	return nil
}

func (o Options) validate() error {
	if o.SecretNamespace == "" {
		return errors.New("Secret namespace must not be empty")
	}
	if o.SecretName == "" {
		return errors.New("Secret name must not be empty")
	}
	if o.OutputDirectory == "" {
		return errors.New("output directory must not be empty")
	}
	return nil
}

func credentialsFromSecret(data map[string][]byte, now time.Time) (credentialFiles, error) {
	certificate, err := requiredData(data, CertificateKey)
	if err != nil {
		return credentialFiles{}, err
	}
	privateKey, err := requiredData(data, PrivateKeyKey)
	if err != nil {
		return credentialFiles{}, err
	}
	ca, err := requiredData(data, CAKey)
	if err != nil {
		return credentialFiles{}, err
	}
	if len(certificate)+len(privateKey)+len(ca) > maxCredentialBytes {
		return credentialFiles{}, fmt.Errorf("credential data exceeds %d bytes", maxCredentialBytes)
	}

	pair, err := tls.X509KeyPair(certificate, privateKey)
	if err != nil {
		return credentialFiles{}, fmt.Errorf("parse client certificate and private key: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return credentialFiles{}, fmt.Errorf("parse client leaf certificate: %w", err)
	}
	if now.Before(leaf.NotBefore) {
		return credentialFiles{}, fmt.Errorf("client certificate is not valid before %s", leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if !now.Before(leaf.NotAfter) {
		return credentialFiles{}, fmt.Errorf("client certificate expired at %s", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if !allowsClientAuth(leaf.ExtKeyUsage) {
		return credentialFiles{}, errors.New("client certificate does not allow ClientAuth")
	}
	if err := validateCertificatesPEM(ca); err != nil {
		return credentialFiles{}, fmt.Errorf("parse CA bundle: %w", err)
	}

	return credentialFiles{
		certificate: append([]byte(nil), certificate...),
		privateKey:  append([]byte(nil), privateKey...),
		ca:          append([]byte(nil), ca...),
	}, nil
}

func requiredData(data map[string][]byte, key string) ([]byte, error) {
	value := data[key]
	if len(value) == 0 {
		return nil, fmt.Errorf("Secret data %q must not be empty", key)
	}
	return value, nil
}

func allowsClientAuth(usages []x509.ExtKeyUsage) bool {
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

func validateCertificatesPEM(contents []byte) error {
	found := false
	for len(bytes.TrimSpace(contents)) > 0 {
		contents = bytes.TrimSpace(contents)
		block, rest := pem.Decode(contents)
		if block == nil {
			return errors.New("invalid PEM data")
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("unexpected PEM block %q", block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return err
		}
		found = true
		contents = rest
	}
	if !found {
		return errors.New("no certificates found")
	}
	return nil
}

func writeCredentialFiles(outputDirectory string, files credentialFiles) (err error) {
	if err := os.MkdirAll(outputDirectory, 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.Chmod(outputDirectory, 0o700); err != nil {
		return fmt.Errorf("set output directory permissions: %w", err)
	}

	outputs := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{name: CertificateFile, data: files.certificate, mode: 0o444},
		{name: PrivateKeyFile, data: files.privateKey, mode: 0o400},
		{name: CAFile, data: files.ca, mode: 0o444},
	}
	defer func() {
		if err == nil {
			return
		}
		for _, output := range outputs {
			path := filepath.Join(outputDirectory, output.name)
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove partial credential %s: %w", output.name, removeErr))
			}
		}
	}()

	for _, output := range outputs {
		path := filepath.Join(outputDirectory, output.name)
		if err = os.WriteFile(path, output.data, output.mode); err != nil {
			return fmt.Errorf("write %s: %w", output.name, err)
		}
	}
	return nil
}
