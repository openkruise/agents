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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestOptionsFromEnvironment(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		secretName  string
		expectError string
	}{
		{name: "valid", namespace: "identity-system", secretName: "gateway-client-cert"},
		{name: "missing namespace", secretName: "gateway-client-cert", expectError: "namespace"},
		{name: "missing name", namespace: "identity-system", expectError: "name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envSecretNamespace, tt.namespace)
			t.Setenv(envSecretName, tt.secretName)
			opts, err := OptionsFromEnvironment()
			assertErrorContains(t, err, tt.expectError)
			if tt.expectError == "" {
				if opts.SecretNamespace != tt.namespace || opts.SecretName != tt.secretName || opts.OutputDirectory != DefaultOutputDirectory {
					t.Fatalf("OptionsFromEnvironment() = %#v", opts)
				}
			}
		})
	}
}

func TestOptionsValidate(t *testing.T) {
	tests := []struct {
		name        string
		opts        Options
		expectError string
	}{
		{name: "valid", opts: validOptions()},
		{name: "missing namespace", opts: Options{SecretName: "secret", OutputDirectory: "/tmp/certs"}, expectError: "namespace"},
		{name: "missing name", opts: Options{SecretNamespace: "namespace", OutputDirectory: "/tmp/certs"}, expectError: "name"},
		{name: "missing output directory", opts: Options{SecretNamespace: "namespace", SecretName: "secret"}, expectError: "output directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validate()
			assertErrorContains(t, err, tt.expectError)
		})
	}
}

func TestCredentialsFromSecret(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	valid := newCredentialData(t, now.Add(-time.Hour), now.Add(time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	other := newCredentialData(t, now.Add(-time.Hour), now.Add(time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})

	tests := []struct {
		name        string
		data        map[string][]byte
		expectError string
	}{
		{name: "valid client auth", data: cloneData(valid)},
		{name: "valid unrestricted usage", data: newCredentialData(t, now.Add(-time.Hour), now.Add(time.Hour), nil)},
		{name: "valid any usage", data: newCredentialData(t, now.Add(-time.Hour), now.Add(time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageAny})},
		{name: "missing certificate", data: withoutKey(valid, CertificateKey), expectError: CertificateKey},
		{name: "missing private key", data: withoutKey(valid, PrivateKeyKey), expectError: PrivateKeyKey},
		{name: "missing CA", data: withoutKey(valid, CAKey), expectError: CAKey},
		{name: "invalid certificate", data: replaceData(valid, CertificateKey, []byte("invalid")), expectError: "parse client certificate"},
		{name: "mismatched private key", data: replaceData(valid, PrivateKeyKey, other[PrivateKeyKey]), expectError: "private key"},
		{name: "not yet valid", data: newCredentialData(t, now.Add(time.Hour), now.Add(2*time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}), expectError: "not valid before"},
		{name: "expired", data: newCredentialData(t, now.Add(-2*time.Hour), now.Add(-time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}), expectError: "expired"},
		{name: "server auth only", data: newCredentialData(t, now.Add(-time.Hour), now.Add(time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}), expectError: "does not allow ClientAuth"},
		{name: "invalid CA PEM", data: replaceData(valid, CAKey, []byte("invalid")), expectError: "invalid PEM"},
		{name: "unexpected CA block", data: replaceData(valid, CAKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("invalid")})), expectError: "unexpected PEM block"},
		{name: "invalid CA certificate", data: replaceData(valid, CAKey, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid")})), expectError: "malformed certificate"},
		{name: "oversized data", data: replaceData(valid, CAKey, []byte(strings.Repeat("x", maxCredentialBytes))), expectError: "exceeds"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, err := credentialsFromSecret(tt.data, now)
			assertErrorContains(t, err, tt.expectError)
			if tt.expectError == "" {
				if string(files.certificate) != string(tt.data[CertificateKey]) || string(files.privateKey) != string(tt.data[PrivateKeyKey]) || string(files.ca) != string(tt.data[CAKey]) {
					t.Fatal("credentialsFromSecret() did not preserve Secret data")
				}
			}
		})
	}
}

func TestValidateCertificatesPEM(t *testing.T) {
	now := time.Now()
	valid := newCredentialData(t, now.Add(-time.Hour), now.Add(time.Hour), nil)[CAKey]

	tests := []struct {
		name        string
		contents    []byte
		expectError string
	}{
		{name: "single certificate with whitespace", contents: append(append([]byte(" \n"), valid...), []byte("\n ")...)},
		{name: "certificate bundle", contents: append(cloneBytes(valid), valid...)},
		{name: "empty", expectError: "no certificates found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorContains(t, validateCertificatesPEM(tt.contents), tt.expectError)
		})
	}
}

func TestLoad(t *testing.T) {
	now := time.Now()
	data := newCredentialData(t, now.Add(-time.Hour), now.Add(time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "identity", Name: "credentials"},
		Data:       data,
	}

	tests := []struct {
		name        string
		reader      client.Reader
		opts        Options
		expectError string
		wantFiles   bool
	}{
		{
			name:      "writes valid credentials",
			reader:    newFakeReader(t, secret.DeepCopy()),
			opts:      Options{SecretNamespace: "identity", SecretName: "credentials"},
			wantFiles: true,
		},
		{
			name:        "Secret not found",
			reader:      newFakeReader(t),
			opts:        Options{SecretNamespace: "identity", SecretName: "credentials"},
			expectError: "get runtime mTLS Secret",
		},
		{
			name:        "invalid Secret",
			reader:      newFakeReader(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "identity", Name: "credentials"}}),
			opts:        Options{SecretNamespace: "identity", SecretName: "credentials"},
			expectError: "validate runtime mTLS Secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.OutputDirectory = t.TempDir()
			err := Load(context.Background(), tt.reader, tt.opts)
			assertErrorContains(t, err, tt.expectError)
			if tt.wantFiles {
				assertCredentialFile(t, tt.opts.OutputDirectory, CertificateFile, data[CertificateKey], 0o444)
				assertCredentialFile(t, tt.opts.OutputDirectory, PrivateKeyFile, data[PrivateKeyKey], 0o400)
				assertCredentialFile(t, tt.opts.OutputDirectory, CAFile, data[CAKey], 0o444)
			}
		})
	}
}

func TestLoadValidationAndWriteErrors(t *testing.T) {
	now := time.Now()
	data := newCredentialData(t, now.Add(-time.Hour), now.Add(time.Hour), nil)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "identity", Name: "credentials"}, Data: data}
	reader := newFakeReader(t, secret)
	filePath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(filePath, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	tests := []struct {
		name        string
		reader      client.Reader
		opts        Options
		expectError string
	}{
		{name: "nil reader", opts: validOptions(), expectError: "must not be nil"},
		{name: "invalid options", reader: reader, opts: Options{}, expectError: "namespace"},
		{name: "output path is file", reader: reader, opts: Options{SecretNamespace: "identity", SecretName: "credentials", OutputDirectory: filePath}, expectError: "create output directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorContains(t, Load(context.Background(), tt.reader, tt.opts), tt.expectError)
		})
	}
}

func validOptions() Options {
	return Options{SecretNamespace: "identity", SecretName: "credentials", OutputDirectory: "/tmp/certs"}
}

func newFakeReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestWriteCredentialFilesErrors(t *testing.T) {
	files := credentialFiles{certificate: []byte("certificate"), privateKey: []byte("key"), ca: []byte("ca")}
	tests := []struct {
		name         string
		blockedFile  string
		cleanupError bool
		expectError  string
	}{
		{name: "certificate write", blockedFile: CertificateFile, expectError: "write " + CertificateFile},
		{name: "private key write", blockedFile: PrivateKeyFile, expectError: "write " + PrivateKeyFile},
		{name: "CA write", blockedFile: CAFile, expectError: "write " + CAFile},
		{name: "partial file cleanup", blockedFile: CertificateFile, cleanupError: true, expectError: "remove partial credential"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			blockedPath := filepath.Join(directory, tt.blockedFile)
			if err := os.Mkdir(blockedPath, 0o700); err != nil {
				t.Fatalf("Mkdir() error = %v", err)
			}
			if tt.cleanupError {
				if err := os.WriteFile(filepath.Join(blockedPath, "keep"), []byte("data"), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}
			err := writeCredentialFiles(directory, files)
			assertErrorContains(t, err, tt.expectError)
			for _, name := range []string{CertificateFile, PrivateKeyFile, CAFile} {
				if tt.cleanupError && name == tt.blockedFile {
					continue
				}
				if _, statErr := os.Stat(filepath.Join(directory, name)); !os.IsNotExist(statErr) {
					t.Errorf("credential file %s remains after error: %v", name, statErr)
				}
			}
		})
	}
}

func newCredentialData(t *testing.T, notBefore, notAfter time.Time, usages []x509.ExtKeyUsage) map[string][]byte {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey(CA) error = %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             notBefore.Add(-time.Hour),
		NotAfter:              notAfter.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("CreateCertificate(CA) error = %v", err)
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey(client) error = %v", err)
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "sandbox-gateway"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usages,
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("CreateCertificate(client) error = %v", err)
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey() error = %v", err)
	}
	return map[string][]byte{
		CertificateKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}),
		PrivateKeyKey:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}),
		CAKey:          pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
	}
}

func cloneData(data map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(data))
	for key, value := range data {
		result[key] = cloneBytes(value)
	}
	return result
}

func withoutKey(data map[string][]byte, key string) map[string][]byte {
	result := cloneData(data)
	delete(result, key)
	return result
}

func replaceData(data map[string][]byte, key string, value []byte) map[string][]byte {
	result := cloneData(data)
	result[key] = value
	return result
}

func cloneBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}

func assertCredentialFile(t *testing.T, directory, name string, want []byte, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(directory, name)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", name, err)
	}
	if string(contents) != string(want) {
		t.Errorf("%s contents differ from Secret data", name)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", name, err)
	}
	if info.Mode().Perm() != mode {
		t.Errorf("%s mode = %o, want %o", name, info.Mode().Perm(), mode)
	}
}

func assertErrorContains(t *testing.T, err error, expectError string) {
	t.Helper()
	if expectError == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), expectError) {
		t.Fatalf("error = %v, want containing %q", err, expectError)
	}
}
