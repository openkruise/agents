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

package sandboxroute

import (
	"errors"

	"github.com/go-logr/logr"

	"github.com/openkruise/agents/pkg/utils"
)

// LogMutation logs the outcome of a route mutation.
func LogMutation(logger logr.Logger, operation string, route Route, result MutationResult) {
	logger = logger.WithValues("operation", operation, "route", route).V(utils.DebugLogLevel)
	if result.Result == EventResultInvalid {
		logger.Error(
			errors.New(string(result.Reason)),
			"route mutation rejected",
		)
		return
	}
	if result.Result == EventResultApplied && result.Reason == ReasonIDTakeover {
		logger.Error(
			errors.New(string(result.Reason)),
			"route mutation ID takeover",
			"reason", result.Reason,
		)
		return
	}
	if result.Result == EventResultApplied && operation == "upsert" {
		logger.Info(
			"route mutation completed",
			"result", result.Result,
			"reason", result.Reason,
		)
		return
	}
	logger.Info(
		"route mutation completed",
		"result", result.Result,
		"reason", result.Reason,
	)
}
