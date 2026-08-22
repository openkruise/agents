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

package runtimeprovider

import (
	"net/http"
	"time"
)

// settings collects the values every Option mutates. It is unexported: both
// implementations read it through their own constructors so a caller of
// NewProvider never has to know which fields a given Kind actually consumes.
type settings struct {
	httpClient *http.Client
	timeout    time.Duration
}

func newSettings(opts []Option) settings {
	s := settings{timeout: defaultTimeout}
	for _, opt := range opts {
		opt(&s)
	}
	if s.httpClient == nil {
		s.httpClient = &http.Client{}
	}
	return s
}

// defaultTimeout bounds a single Exec/WriteFile/ReadFile/Init call when the
// caller does not override it via WithTimeout. Command execution can
// legitimately run long, so this is intentionally generous; a caller running
// short-lived commands should pass a context deadline instead of relying on
// this floor.
const defaultTimeout = 60 * time.Second

// Option customizes a Provider built by NewProvider.
type Option func(*settings)

// WithHTTPClient overrides the transport used to reach the daemon. Useful for
// injecting a TLS-configured client (e.g. runtime mTLS) or a test double.
func WithHTTPClient(c *http.Client) Option {
	return func(s *settings) {
		if c != nil {
			s.httpClient = c
		}
	}
}

// WithTimeout bounds every call made by the Provider when the caller's
// context carries no earlier deadline. Values <= 0 are ignored.
func WithTimeout(d time.Duration) Option {
	return func(s *settings) {
		if d > 0 {
			s.timeout = d
		}
	}
}
