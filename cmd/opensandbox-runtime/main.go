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
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/agent-runtime/opensandbox"
	"github.com/openkruise/agents/pkg/runtime/envd"
)

func main() {
	listen := flag.String("listen", ":44772", "HTTP execution listener; ingress owns external TLS termination")
	backend := flag.String("envd-url", "http://127.0.0.1:49983", "Native envd endpoint in the same Pod")
	state := flag.String("credential-file", "/var/run/opensandbox/access-token", "Sandbox-local initialization credential file")
	klog.InitFlags(nil)
	flag.Parse()
	handler, err := opensandbox.New(envd.Client{BaseURL: *backend, HTTP: &http.Client{}}, *state)
	if err != nil {
		klog.ErrorS(err, "initialize execution service")
		os.Exit(1)
	}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			klog.ErrorS(err, "serve execution API")
			os.Exit(1)
		}
	case <-ctx.Done():
		handler.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			klog.ErrorS(err, "drain execution API")
			if err := server.Close(); err != nil {
				klog.ErrorS(err, "close execution API")
			}
		}
	}
}
