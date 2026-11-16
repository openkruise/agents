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

package e2b

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestE2BExcludesSessionServiceRoutes(t *testing.T) {
	sc := NewController(ControllerOptions{})
	sc.registerRoutes()
	for _, prefix := range []string{"/v1/sessionservice/", "/kruise/api/v1alpha1/sessionservices/"} {
		for _, route := range []struct{ method, suffix string }{
			{"POST", "/traffic-access-token"}, {"GET", "/sessions/session-a"}, {"GET", "/sessions"}, {"POST", "/sessions/session-a:close"},
		} {
			_, pattern := sc.mux.Handler(httptest.NewRequest(route.method, prefix+"abcde234abcde"+route.suffix, nil))
			require.Empty(t, pattern, route.method+" "+prefix+route.suffix)
		}
	}
}
