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

package tls

import (
	"crypto/x509"
	"testing"
)

// TestCreateSelfSignedTLSCertificate verifies the helper produces a parseable
// X.509 certificate with a non-empty private key. RSA-4096 generation is slow
// (~1s) but happens once at server startup so the cost is acceptable.
func TestCreateSelfSignedTLSCertificate(t *testing.T) {
	cert, err := CreateSelfSignedTLSCertificate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cert.PrivateKey == nil {
		t.Error("expected non-nil private key")
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("expected at least one certificate in the chain")
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("certificate must parse: %v", err)
	}
	if parsed.Subject.Organization[0] == "" {
		t.Error("expected non-empty organization in subject")
	}
}
