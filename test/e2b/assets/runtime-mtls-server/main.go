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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	caPEM, err := os.ReadFile("/tls/ca.crt")
	if err != nil {
		log.Fatalf("read client CA: %v", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		log.Fatal("parse client CA")
	}

	go func() {
		plaintextServer := &http.Server{
			Addr: ":8080",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := fmt.Fprint(w, "runtime-plaintext-ok"); err != nil {
					log.Printf("write plaintext response: %v", err)
				}
			}),
		}
		log.Fatal(plaintextServer.ListenAndServe())
	}()

	server := &http.Server{
		Addr: ":49983",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := fmt.Fprint(w, "runtime-mtls-ok"); err != nil {
				log.Printf("write mTLS response: %v", err)
			}
		}),
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs:  clientCAs,
		},
	}
	log.Fatal(server.ListenAndServeTLS("/tls/tls.crt", "/tls/tls.key"))
}
