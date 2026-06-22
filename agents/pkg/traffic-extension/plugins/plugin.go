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

// Package plugins defines the contract for traffic-extension request-handling
// plugins. The handlers package walks the rule chain and asks each registered
// plugin whether it wants to act on the rule. Plugins return a Result that
// tells the handler whether to short-circuit (Immediate), accumulate header
// mutations (Mutate), or skip (Continue).
package plugins

import (
	"context"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/traffic-extension/model"
)

// Action is the disposition a plugin reports back to the handler.
type Action int

const (
	// ActionContinue means the plugin did not act on the rule; the handler
	// should keep walking remaining plugins / rules.
	ActionContinue Action = iota
	// ActionImmediate means the plugin produced a terminal response. The
	// handler must return Responses immediately and skip remaining plugins
	// and rules.
	ActionImmediate
	// ActionMutate means the plugin produced header mutations to apply.
	// The handler accumulates them and continues walking. The handler
	// guarantees the same plugin is invoked at most once per request in
	// this mode (first matching rule wins).
	ActionMutate
	// ActionRecord means the plugin claimed responsibility for the rule
	// but is deferring its side effect (e.g. an upstream RPC) until the
	// rule scan confirms no terminal Immediate has fired. The handler
	// records the matched rule and, after the scan finishes without an
	// Immediate, calls the plugin's Finalize method to produce the actual
	// mutation. Plugins that return ActionRecord MUST also implement the
	// Finalizer interface; the handler treats Record-without-Finalizer as
	// a programmer error.
	ActionRecord
)

// Result is the value returned by a plugin's OnRequestHeaders.
//
// Err is an optional non-fatal error a plugin may surface when it decides
// to skip a rule but still wants the orchestrator's audit log to record
// why. The handler never propagates Err back to the gRPC caller — plugins
// that need to abort the request must continue to return an error from
// OnRequestHeaders/Finalize.
type Result struct {
	Action    Action
	Responses []*extProcPb.ProcessingResponse
	Err       error
}

// ContinueResult is a convenience constructor for "skip" results.
func ContinueResult() Result {
	return Result{Action: ActionContinue}
}

// ImmediateResult builds a terminal result with the given responses.
func ImmediateResult(responses ...*extProcPb.ProcessingResponse) Result {
	return Result{Action: ActionImmediate, Responses: responses}
}

// MutateResult builds a header-mutation result with the given responses.
func MutateResult(responses ...*extProcPb.ProcessingResponse) Result {
	return Result{Action: ActionMutate, Responses: responses}
}

// RecordResult is a convenience constructor for "matched but deferred" results.
// The plugin must also implement Finalizer.
func RecordResult() Result {
	return Result{Action: ActionRecord}
}

// RequestContext is the per-request data the handler hands to every plugin.
// Plugins should treat it as read-only.
type RequestContext struct {
	// Headers is the lowercase-keyed Envoy request header map.
	Headers map[string]string
	// Info is the parsed (host, path, method) tuple used for matching.
	Info model.RequestInfo
	// PodNN identifies the source pod (for logging).
	PodNN types.NamespacedName
	// Profile is the SecurityProfile that owns the rule being evaluated.
	Profile *model.SecurityProfile
}

// Plugin is the interface every functional module implements.
//
// Plugins inspect a single SecurityRule on each invocation and decide whether
// they act. Implementations must be safe for concurrent use.
type Plugin interface {
	// Name returns a stable identifier (used as a per-request mutate-once
	// key). Two plugins must never share a name.
	Name() string

	// OnRequestHeaders is invoked once per matching rule per request. The
	// plugin should return ActionContinue if the rule's action is not
	// relevant to it.
	OnRequestHeaders(ctx context.Context, rctx *RequestContext, rule *model.SecurityRule) (Result, error)

	// Finalize is called after the rule scan completes without an
	// Immediate response, for plugins that returned ActionRecord from
	// OnRequestHeaders. Finalize MUST return ActionContinue, ActionMutate,
	// or ActionImmediate; ActionRecord is not valid here. Plugins that
	// never return ActionRecord should return ContinueResult().
	Finalize(ctx context.Context, rctx *RequestContext, rule *model.SecurityRule) (Result, error)
}
