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

package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"time"
)

const (
	keyID          = "e2e-key"
	identityDir    = "/identity"
	signingKeyPath = identityDir + "/signing.key"
	tlsCertPath    = identityDir + "/tls.crt"
	tlsKeyPath     = identityDir + "/tls.key"
)

type sandboxClaims struct {
	SandboxID  string `json:"sandboxId"`
	SandboxUID string `json:"sandboxUid"`
}

type tokenClaims struct {
	Issuer    string        `json:"iss"`
	Subject   string        `json:"sub"`
	IssuedAt  int64         `json:"iat"`
	NotBefore int64         `json:"nbf"`
	Expiry    int64         `json:"exp"`
	Sandbox   sandboxClaims `json:"sandbox"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	issuer := os.Getenv("OIDC_ISSUER")
	if issuer == "" {
		return errors.New("OIDC_ISSUER must not be empty")
	}
	privateKey, err := loadPrivateKey(signingKeyPath)
	if err != nil {
		return err
	}
	if len(os.Args) > 1 && os.Args[1] == "issue" {
		return issueToken(issuer, privateKey, os.Args[2:])
	}
	return serve(issuer, privateKey)
}

func serve(issuer string, privateKey *rsa.PrivateKey) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{
			"issuer":   issuer,
			"jwks_uri": issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": keyID,
				"n":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
			}},
		})
	})

	server := &http.Server{
		Addr:              ":8443",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return server.ListenAndServeTLS(tlsCertPath, tlsKeyPath)
}

func issueToken(issuer string, privateKey *rsa.PrivateKey, args []string) error {
	flags := flag.NewFlagSet("issue", flag.ContinueOnError)
	sandboxID := flags.String("sandbox-id", "", "sandbox ID")
	sandboxUID := flags.String("sandbox-uid", "", "sandbox UID")
	expired := flags.Bool("expired", false, "issue an expired token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *sandboxID == "" || *sandboxUID == "" {
		return errors.New("sandbox-id and sandbox-uid must not be empty")
	}

	now := time.Now()
	issuedAt := now.Add(-time.Second)
	expiry := now.Add(5 * time.Minute)
	if *expired {
		issuedAt = now.Add(-10 * time.Minute)
		expiry = now.Add(-2 * time.Minute)
	}
	claims := tokenClaims{
		Issuer:    issuer,
		Subject:   "jwt-e2e-client",
		IssuedAt:  issuedAt.Unix(),
		NotBefore: issuedAt.Unix(),
		Expiry:    expiry.Unix(),
		Sandbox: sandboxClaims{
			SandboxID:  *sandboxID,
			SandboxUID: *sandboxUID,
		},
	}
	token, err := signToken(privateKey, claims)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, token)
	return err
}

func signToken(privateKey *rsa.PrivateKey, claims tokenClaims) (string, error) {
	headerJSON, err := json.Marshal(map[string]string{"alg": "RS256", "kid": keyID, "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("encode JWT header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode JWT claims: %w", err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	block, _ := pem.Decode(contents)
	if block == nil {
		return nil, errors.New("decode signing key PEM")
	}
	if block.Type == "RSA PRIVATE KEY" {
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#1 signing key: %w", err)
		}
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 signing key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("signing key is not RSA")
	}
	return key, nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}
