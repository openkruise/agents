# Gateway UUID credentials and fail-closed authentication

## Problem

E2B pool-based creation previously generated a UUID access token only when
runtime initialization was requested. With `skip-init-runtime=true`, a delivered
Running Sandbox could therefore have no access token. The gateway's existing
UUID authentication skips validation for routes without tokens, even when
`enable-auth=true`.

## Decision

Generate the existing UUID credential for each E2B claim delivery regardless of
`skip-init-runtime`. Preserve the current generation point before claim retries.
Return the UUID in the existing `envdAccessToken` response field and require
clients to send it in `x-access-token` when gateway UUID authentication is on.

Always pass the UUID in `ClaimSandboxOptions.AccessToken`. For ordinary
initialization, also pass the same value in `InitRuntime.AccessToken` for the
runtime `/init` request. For skipped initialization, leave `InitRuntime` nil.
The SandboxClaim controller allocates the outer token in `buildClaimOptions`,
before runtime initialization decisions. Skipped initialization and templates
without a runtime therefore still receive a gateway credential. If a runtime is
initialized, `applyInitRuntimeOptions` reuses that token for the `/init` request;
it never generates another token. The existing token allocation granularity
within a SandboxClaim batch is unchanged.

Infra writes the runtime-access-token annotation exclusively from the outer
`AccessToken`, independently of whether initialization is requested. It still
persists the optional `InitRuntime` request unchanged. Both annotations are
written during the claim mutation, before the claimed route is published.
The shared route projection needs no changes.
The outer token is excluded from JSON serialization so logging that field does
not expose it; the existing runtime request DTO and its token field are retained.

Skipping initialization still means no `/init` request and no newly recorded
init-runtime-request annotation. The runtime does not learn a credential merely
because the control plane persists it or a client sends its header. For a fresh,
uninitialized runtime, this credential protects the gateway boundary only.

## Gateway admission policy

For routes without JWT enforcement, `enable-auth=true` requires both a nonempty
route access token and a matching `x-access-token` request header. A missing
route credential, missing request credential, or mismatch returns the existing
`401` / `unauthorized` local reply before upstream selection or wake handling.
Two empty tokens must never count as a successful match. Nonempty credentials
continue to use constant-time comparison.

Explicitly disabled UUID authentication (`enable-auth=false`) continues to allow
non-JWT routes without checking their UUID token. JWT-required routes retain
their separate verification and fail-closed behavior regardless of this switch.

## Compatibility and scope

- `enable-auth=true` is required for gateway UUID enforcement. This change does
  not silently enable authentication in deployments that disabled it.
- JWT-opted-in routes retain their existing JWT enforcement.
- Clients that previously omitted the token for skipped-init claims must now
  send the UUID when gateway UUID authentication is enabled. E2B clients receive
  it in the create response; SandboxClaim clients read the token annotation on
  their claimed Sandbox.
- A runtime with an independently configured token is not reconfigured. A new
  gateway UUID may differ from that token; supporting independent credentials
  for both hops requires a separate design.
- Existing tokenless Sandboxes are not migrated and are now rejected by gateways
  with UUID authentication enabled. Provision replacement Sandboxes through the
  updated claim paths before relying on their credentials. Clone behavior is
  unchanged, but any tokenless clone is likewise rejected by an auth-enabled
  gateway. This intentionally removes the former empty-token allow policy.
- Direct Pod access is outside this gateway protection boundary.

## Validation

Exercise ordinary and skipped-init creation with a recording HTTP runtime.
Verify each delivery returns a distinct UUID matching the persisted annotation
and projected Running route. Ordinary creation must initialize with that UUID;
skipped creation must make no runtime initialization request and record no init
request annotation. Keep the gateway tests for missing, wrong, and matching
UUID headers as the traffic-admission checks.
Filter tests also require a route without a token to return 401 for missing,
empty, and arbitrary request tokens, including customized routing and the UUID
baseline in JWT mode. Rejected requests must not select an upstream. Disabled
authentication and JWT-required route tests retain their existing expectations.
SandboxClaim option tests require a nonempty outer token in every successful
case, including skipped initialization and templates without a runtime, while
retaining their existing nil-init checks and token equality checks when initialized.
