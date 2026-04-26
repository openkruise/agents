/*
Copyright 2025.

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

package tracing

import "go.opentelemetry.io/otel/attribute"

const (
	SpanClaimSandbox     = "sandbox.claim"
	SpanCloneSandbox     = "sandbox.clone"
	SpanPauseSandbox     = "sandbox.pause"
	SpanDeleteSandbox    = "sandbox.delete"
	SpanGetSandbox       = "sandbox.get"
	SpanListSandboxes    = "sandboxes.list"
	SpanCreateCheckpoint = "checkpoint.create"
	SpanDeleteCheckpoint = "checkpoint.delete"
)

const (
	AttrSandboxID          = "sandbox.id"
	AttrSandboxName        = "sandbox.name"
	AttrSandboxState       = "sandbox.state"
	AttrSandboxNode        = "sandbox.node"
	AttrSandboxNamespace   = "sandbox.namespace"
	AttrTemplateID         = "sandbox.template_id"
	AttrCheckpointID       = "sandbox.checkpoint_id"
	AttrUserID             = "sandbox.user_id"
	AttrPauseTimeout       = "sandbox.pause_timeout"
	AttrClaimTimeout       = "sandbox.claim_timeout"
	AttrCloneTimeout       = "sandbox.clone_timeout"
	AttrWaitReadyTimeout   = "sandbox.wait_ready_timeout"
	AttrAccessTokenSet     = "sandbox.access_token_set"
	AttrInitRuntimeSkipped = "sandbox.init_runtime_skipped"
)

const (
	AttrInfraOperation = "infra.operation"
	AttrPoolSize       = "infra.pool_size"
	AttrCandidateCount = "infra.candidate_count"
)

const (
	AttrErrorCode    = "error.code"
	AttrErrorMessage = "error.message"
	AttrErrorType    = "error.type"
)

const (
	EventTemplateFound    = "template.found"
	EventCheckpointFound  = "checkpoint.found"
	EventSandboxLocked    = "sandbox.locked"
	EventSandboxCreated   = "sandbox.created"
	EventRouteSynced      = "route.synced"
	EventPauseRequested   = "pause.requested"
	EventSandboxRefreshed = "sandbox.refreshed"
)

func SandboxAttributes(sandboxID, name, namespace, user string, state interface{}) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(AttrSandboxID, sandboxID),
		attribute.String(AttrUserID, user),
	}
	if name != "" {
		attrs = append(attrs, attribute.String(AttrSandboxName, name))
	}
	if namespace != "" {
		attrs = append(attrs, attribute.String(AttrSandboxNamespace, namespace))
	}
	if state != nil {
		attrs = append(attrs, attribute.String(AttrSandboxState, toStringAttr(state)))
	}
	return attrs
}

func toStringAttr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
