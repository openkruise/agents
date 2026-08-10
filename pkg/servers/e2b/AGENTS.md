# E2B API

This directory implements the E2B-compatible API layer. It owns protocol
behavior and delegates protocol-independent use cases to Manager.

## Protocol Contract

- Before changing endpoints, status codes, request/response fields, or
  validation, inspect the relevant section of the upstream
  [E2B OpenAPI specification](https://github.com/e2b-dev/E2B/blob/main/spec/openapi.yml).
- Register every E2B endpoint through `RegisterE2BRoute` so the native and
  customized paths stay equivalent.
- Request/response types and protocol validation belong in `models`.
  Compatibility-specific error bodies and HTTP status mapping stay in this
  package.
- Preserve the typed quota-exceeded to HTTP `403` mapping. Backend failures
  remain Manager concerns and must not leak implementation details into API
  responses.
- New work must call Manager interfaces instead of adding direct API-to-Infra
  access.

## Authentication And Authorization

- `CheckApiKey` authenticates the caller and enforces sandbox or volume
  ownership. When authentication is disabled, the canonical anonymous caller
  has admin privileges.
- API-key creation and deletion permissions are enforced by
  `CheckCreateAPIKeyPermission` and `CheckDeleteAPIKeyPermission` after
  `CheckApiKey`.
- Team name is the authorization and namespace identity. Team UUIDs are
  compatibility/display metadata and must not drive lookup, equality,
  authorization, or namespace selection.
- Namespace-backed team names must remain valid for Sandbox ID encoding,
  including the reserved `--` separator rule.
- List visibility and delete authorization must remain consistent for
  sandboxes, snapshots, templates, and API keys.

## Timeout Behavior

- Pause gives timed sandboxes a paused-retention deadline; never-timeout
  sandboxes keep timeout fields nil.
- Resume applies the effective timeout after the resume floor; never-timeout
  sandboxes remain without a deadline.
- Connect on a running sandbox is extend-only. Connect on a paused sandbox
  applies the resume floor during the state transition, then performs an
  extend-only timeout update. Racing shorter requests must not shrink the
  longest accepted deadline.
- SetTimeout applies only to running sandboxes; state conflicts map to HTTP
  `409`.
- Keep synchronous claim/clone/wait deadlines distinct from the sandbox
  lifecycle timeout.
