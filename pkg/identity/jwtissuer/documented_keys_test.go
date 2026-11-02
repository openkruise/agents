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

package jwtissuer

import (
	"crypto"
	"encoding/base64"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// TestDocumentedKeyCommandsProduceUsableKeys runs each openssl invocation the
// signing key guide gives an operator, then drives the real loader and issuer
// with whatever came out and verifies a token against the published key set.
//
// A guide is a set of claims. Reading one only checks that the commands look
// plausible, and an operator following a command that produces a key this
// loader rejects has no way to tell whose fault it is.
func TestDocumentedKeyCommandsProduceUsableKeys(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not on PATH")
	}

	cases := []struct {
		name string
		// args is the command verbatim from the guide, minus the output path.
		args    []string
		wantAlg jose.SignatureAlgorithm
	}{
		{
			name:    "the recommended P-256 key",
			args:    []string{"ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out"},
			wantAlg: jose.ES256,
		},
		{
			name:    "the P-384 alternative",
			args:    []string{"ecparam", "-name", "secp384r1", "-genkey", "-noout", "-out"},
			wantAlg: jose.ES384,
		},
		{
			name:    "the P-521 alternative",
			args:    []string{"ecparam", "-name", "secp521r1", "-genkey", "-noout", "-out"},
			wantAlg: jose.ES512,
		},
		{
			name:    "the ed25519 alternative",
			args:    []string{"genpkey", "-algorithm", "ed25519", "-out"},
			wantAlg: jose.EdDSA,
		},
		{
			name:    "the RSA 3072 alternative",
			args:    []string{"genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:3072", "-out"},
			wantAlg: jose.RS256,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "signing.key")
			command := exec.Command("openssl", append(tc.args, path)...)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("the documented command failed: %v\n%s", err, output)
			}

			signingKey, err := LoadSigningKeyFromFile(path)
			if err != nil {
				t.Fatalf("the loader rejected a key the guide tells an operator to generate: %v", err)
			}
			if signingKey.KeyID == "" {
				t.Error("the loaded key has no key ID, so the JWKS cannot name it")
			}

			issuer, err := New("https://identity.example.com", signingKey)
			if err != nil {
				t.Fatalf("the issuer rejected the loaded key: %v", err)
			}

			keySet, err := issuer.PublicKeySet()
			if err != nil {
				t.Fatalf("PublicKeySet: %v", err)
			}
			if len(keySet.Keys) != 1 {
				t.Fatalf("published %d keys, want 1", len(keySet.Keys))
			}
			if got := jose.SignatureAlgorithm(keySet.Keys[0].Algorithm); got != tc.wantAlg {
				t.Errorf("published algorithm is %q, want %q", got, tc.wantAlg)
			}
			if !keySet.Keys[0].IsPublic() {
				t.Error("the published key carries private material")
			}

			// The point of the key is a token somebody else can verify, so the
			// check ends where a verifier would start: at the JWKS.
			raw, _, err := issuer.IssueAgentToken(AgentBinding{
				Namespace:         "default",
				AgentIdentityName: "checkout-agent",
				SandboxUID:        "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
			}, time.Minute)
			if err != nil {
				t.Fatalf("IssueAgentToken: %v", err)
			}

			parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{tc.wantAlg})
			if err != nil {
				t.Fatalf("parse the issued token: %v", err)
			}
			var claims jwt.Claims
			if err := parsed.Claims(&keySet, &claims); err != nil {
				t.Fatalf("verify the issued token against the published key set: %v", err)
			}
			if want := "agent:default:checkout-agent"; claims.Subject != want {
				t.Errorf("sub is %q, want %q", claims.Subject, want)
			}
		})
	}
}

// TestUndersizedRSAKeyIsRefused covers the one size the guide warns about.
// RFC 7518 section 3.3 requires 2048 bits for RS256 and no verifier in the path
// checks the modulus, so the issuer is the only thing that can decline.
func TestUndersizedRSAKeyIsRefused(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not on PATH")
	}

	path := filepath.Join(t.TempDir(), "weak.key")
	command := exec.Command("openssl", "genpkey", "-algorithm", "RSA",
		"-pkeyopt", "rsa_keygen_bits:1024", "-out", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("openssl would not generate a 1024-bit RSA key: %v\n%s", err, output)
	}

	signingKey, err := LoadSigningKeyFromFile(path)
	if err != nil {
		// Refusing at load is also correct, as long as it is refused.
		return
	}
	if _, err := New("https://identity.example.com", signingKey); err == nil {
		t.Fatal("the issuer accepted a 1024-bit RSA key, which RS256 does not permit")
	} else if !strings.Contains(err.Error(), "2048") {
		t.Errorf("the refusal does not name the required size: %v", err)
	}
}

// TestDocumentedPEMEncodingsAreAllRead covers the guide's claim that PKCS#8,
// PKCS#1 and SEC1 are all accepted, so an operator does not have to convert a
// key after generating it.
func TestDocumentedPEMEncodingsAreAllRead(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not on PATH")
	}

	cases := []struct {
		name string
		// generate writes a key at path in one specific encoding.
		generate [][]string
	}{
		{
			name:     "SEC1 EC, which is what ecparam -genkey writes",
			generate: [][]string{{"ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out"}},
		},
		{
			name:     "PKCS#8 EC",
			generate: [][]string{{"genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256", "-out"}},
		},
		{
			name:     "PKCS#8 RSA",
			generate: [][]string{{"genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048", "-out"}},
		},
		{
			name:     "PKCS#1 RSA, the traditional encoding",
			generate: [][]string{{"genrsa", "-traditional", "-out"}, {"2048"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "signing.key")
			args := append([]string{}, tc.generate[0]...)
			args = append(args, path)
			for _, extra := range tc.generate[1:] {
				args = append(args, extra...)
			}
			if output, err := exec.Command("openssl", args...).CombinedOutput(); err != nil {
				t.Skipf("openssl would not produce this encoding: %v\n%s", err, output)
			}

			header := firstPEMHeader(t, path)
			signingKey, err := LoadSigningKeyFromFile(path)
			if err != nil {
				t.Fatalf("the loader rejected a %s key: %v", header, err)
			}
			if _, err := New("https://identity.example.com", signingKey); err != nil {
				t.Fatalf("the issuer rejected a %s key: %v", header, err)
			}
			t.Logf("accepted PEM block type %q", header)
		})
	}
}

// TestKeyIDIsTheRFC7638Thumbprint checks the guide's claim about where the kid
// comes from, by deriving it independently rather than by reading the code.
//
// The claim matters operationally: it is what lets every replica agree on a kid
// without coordinating, and what makes a changed kid mean a changed key.
func TestKeyIDIsTheRFC7638Thumbprint(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not on PATH")
	}

	path := filepath.Join(t.TempDir(), "signing.key")
	if output, err := exec.Command("openssl", "ecparam", "-name", "prime256v1",
		"-genkey", "-noout", "-out", path).CombinedOutput(); err != nil {
		t.Fatalf("generate key: %v\n%s", err, output)
	}

	signingKey, err := LoadSigningKeyFromFile(path)
	if err != nil {
		t.Fatalf("load key: %v", err)
	}

	// Derived through go-jose's own RFC 7638 implementation, which is a separate
	// path from whatever the loader does.
	public := jose.JSONWebKey{Key: signingKey.PrivateKey.Public()}
	thumbprint, err := public.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatalf("compute thumbprint: %v", err)
	}
	want := base64.RawURLEncoding.EncodeToString(thumbprint)

	if signingKey.KeyID != want {
		t.Errorf("the derived key ID is %q, but the SHA-256 RFC 7638 thumbprint is %q",
			signingKey.KeyID, want)
	}

	// Loading the same material twice must agree, or replicas would not.
	again, err := LoadSigningKeyFromFile(path)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if again.KeyID != signingKey.KeyID {
		t.Errorf("loading the same key twice gave %q then %q", signingKey.KeyID, again.KeyID)
	}

	// And a different key must give a different ID.
	other := filepath.Join(t.TempDir(), "other.key")
	if output, err := exec.Command("openssl", "ecparam", "-name", "prime256v1",
		"-genkey", "-noout", "-out", other).CombinedOutput(); err != nil {
		t.Fatalf("generate the second key: %v\n%s", err, output)
	}
	second, err := LoadSigningKeyFromFile(other)
	if err != nil {
		t.Fatalf("load the second key: %v", err)
	}
	if second.KeyID == signingKey.KeyID {
		t.Error("two different keys derived the same key ID")
	}
}

// TestIssuedTokenCarriesTheKeyIDInItsHeader covers the guide's claim that a
// verifier can pick the right key out of the set, which only works if the kid
// is actually in the JWS header.
func TestIssuedTokenCarriesTheKeyIDInItsHeader(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not on PATH")
	}

	path := filepath.Join(t.TempDir(), "signing.key")
	if output, err := exec.Command("openssl", "ecparam", "-name", "prime256v1",
		"-genkey", "-noout", "-out", path).CombinedOutput(); err != nil {
		t.Fatalf("generate key: %v\n%s", err, output)
	}
	signingKey, err := LoadSigningKeyFromFile(path)
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	issuer, err := New("https://identity.example.com", signingKey)
	if err != nil {
		t.Fatalf("build issuer: %v", err)
	}

	raw, _, err := issuer.IssueAgentToken(AgentBinding{
		Namespace:         "default",
		AgentIdentityName: "checkout-agent",
		SandboxUID:        "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
	}, time.Minute)
	if err != nil {
		t.Fatalf("IssueAgentToken: %v", err)
	}

	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if len(parsed.Headers) != 1 {
		t.Fatalf("token has %d headers, want 1", len(parsed.Headers))
	}
	if got := parsed.Headers[0].KeyID; got != signingKey.KeyID {
		t.Errorf("the JWS header kid is %q, want the published %q", got, signingKey.KeyID)
	}

	keySet, err := issuer.PublicKeySet()
	if err != nil {
		t.Fatalf("PublicKeySet: %v", err)
	}
	if matches := keySet.Key(parsed.Headers[0].KeyID); len(matches) != 1 {
		t.Errorf("the header kid selected %d keys from the published set, want 1", len(matches))
	}
}

// firstPEMHeader reports the PEM block type of the key on disk, so a failure
// names the encoding that was rejected.
func firstPEMHeader(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block, _ := pem.Decode(contents)
	if block == nil {
		t.Fatalf("%s holds no PEM block", path)
	}
	return block.Type
}
