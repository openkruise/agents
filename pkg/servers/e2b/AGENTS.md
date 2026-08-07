# E2B API

This directory implements the E2B-compatible API layer. It owns protocol
behavior and delegates protocol-independent use cases to Manager.

## Protocol Contract

- Before changing endpoints, status codes, request/response fields, or
  validation, inspect the relevant section of the upstream
  [E2B OpenAPI specification](https://github.com/e2b-dev/E2B/blob/main/spec/openapi.yml).
- Keep native and customized endpoint paths behaviorally equivalent.
- Preserve established public error categories and status mappings. Backend
  details remain Manager concerns and must not leak into API responses.
- Preserve the established Sandbox lookup contract: lookup failures remain
  not-found responses and retain their diagnostic message.

## Authentication And Authorization

- Authenticate the caller, enforce resource ownership, and then apply
  operation-specific permissions for API-key creation or deletion. When
  authentication is disabled, the canonical anonymous caller has admin
  privileges.
- Team name is the authorization and namespace identity. Team UUIDs are
  compatibility/display metadata and must not drive lookup, equality,
  authorization, or namespace selection.
- Pass the resolved Sandbox ID through protocol surfaces unchanged. E2B code
  must not generate, migrate, or reverse-parse it.
- Public input cannot set system-owned metadata. Internal resource diagnostics
  are disclosed only after successful lookup and ownership authorization.
- Namespace-backed team names must remain valid for legacy Sandbox ID encoding;
  `--` is reserved as the legacy separator.
- List visibility and delete authorization must remain consistent for
  sandboxes, snapshots, templates, and API keys.

## Timeout Behavior

- An absent paused-retention value uses the API default of `"forever"`;
  accepted writes may persist the resolved value.
- Keep lifecycle deadlines, paused retention, and synchronous operation
  deadlines as distinct contracts. Never-timeout Sandboxes remain without a
  lifecycle deadline.
- Deadline-extending operations must not shorten an already accepted deadline,
  including under concurrent requests.
