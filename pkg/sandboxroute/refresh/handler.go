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

package refresh

import (
	"encoding/json"
	"errors"
	"net/http"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/utils"
)

const (
	Path        = "/refresh"
	DefaultPort = 7789
	// maxRefreshBodyBytes bounds a peer refresh payload; a serialized Route is
	// far below this limit.
	maxRefreshBodyBytes = 1 << 20
)

// RouteMutator applies peer route updates and authoritative deletions.
type RouteMutator interface {
	Upsert(sandboxroute.Route) sandboxroute.MutationResult
	Delete(sandboxroute.Route) sandboxroute.MutationResult
}

// NewHandler creates the shared peer route refresh handler. After every
// mutator call, afterMutation receives its result when the callback is non-nil.
func NewHandler(
	mutator RouteMutator,
	afterMutation func(sandboxroute.MutationResult),
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log := klog.FromContext(r.Context())

		r.Body = http.MaxBytesReader(w, r.Body, maxRefreshBodyBytes)
		var route sandboxroute.Route
		if err := json.NewDecoder(r.Body).Decode(&route); err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				log.Error(err, "route refresh payload exceeds the size limit")
				http.Error(w, "route refresh payload too large", http.StatusRequestEntityTooLarge)
				return
			}
			log.Error(err, "failed to decode route refresh payload")
			http.Error(w, "invalid route refresh payload", http.StatusBadRequest)
			return
		}
		if route.Namespace == "" && route.Name == "" && route.ID != "" {
			log.V(utils.DebugLogLevel).Info("ignoring legacy ID-only peer route refresh", "id", route.ID)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		var result sandboxroute.MutationResult
		if route.State == v1alpha1.SandboxStateDead {
			if route.ResourceVersion == "" {
				http.Error(w, "invalid route refresh payload", http.StatusBadRequest)
				return
			}
			result = mutator.Delete(route)
		} else {
			result = mutator.Upsert(route)
		}
		if afterMutation != nil {
			afterMutation(result)
		}
		log.V(utils.DebugLogLevel+1).Info(
			"route refresh processed",
			"route", route,
			"result", result.Result,
			"reason", result.Reason,
		)
		if result.Result == sandboxroute.EventResultInvalid {
			log.Error(errors.New(string(result.Reason)), "rejected invalid route refresh payload")
			http.Error(w, "invalid route refresh payload", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
