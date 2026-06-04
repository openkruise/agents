# Security Token Refresh Controller

## Role

This package hosts a controller-runtime reconciler that proactively refreshes the
security token of claimed sandboxes shortly before the token recorded in the
sandbox annotation `security.agents.kruise.io/token-status`
(`identity.AgentKeyTokenRefreshStatus`) expires.

It runs inside the **agent-sandbox-controller** binary and is registered via
`pkg/controller/controllers.go`. It is gated by the
`SecurityIdentityProvider` feature gate and by Sandbox CRD discovery, so
clusters without an identity provider behave exactly like before.

## Responsibilities

- Watch `Sandbox` resources, filtered by `needsRefreshPredicate()`:
  - `LabelSandboxIsClaimed == "true"`,
  - non-empty `token-status` annotation,
  - not currently being deleted.
  - On `Update`: only re-enqueue when the `token-status` annotation actually changes
    (timing-based refreshes are driven by `RequeueAfter`, not status churn).
  - `Delete` events are dropped.
- Decode the annotation, compute `refreshAt = expireAt - refreshLeadTime ± jitter`,
  and either `RequeueAfter` until that moment or trigger a refresh.
- Delegate the actual side-effects to `core.Refresher`, which performs:
  1. `identity.IssueSandboxToken`
  2. `identity.PropagateSandboxToken`
  3. patch the sandbox `token-status` annotation (only if 1 and 2 succeeded)
- Emit Kubernetes events (`TokenRefreshed`, `TokenRefreshFailed`,
  `TokenStatusDecodeFailed`, `TokenExpirationInvalid`) so operators can debug
  the pipeline without reading controller logs.

## Extension surface

`Add(mgr)` exposes a `SetupHook` registration point so downstream
distributions (e.g. enterprise builds) can attach extra peer Runnables
alongside the reconciler **without forking this package**:

```go
type SetupHook func(mgr manager.Manager) error
func RegisterSetupHook(h SetupHook)
```

Hooks are called from `init()` in the downstream package, run in
registration order at the tail of `Add()` after the reconciler is wired,
and may freely use `mgr.Add(...)`, `mgr.GetClient()`, etc. The first hook
returning a non-nil error aborts controller setup; the error is wrapped
with a `securitytokenrefresh setup hook #N failed:` prefix so the failure
surface is unambiguous. `nil` hooks are silently dropped.

This is the canonical mechanism to bolt on capabilities such as a CA
bundle sync runnable, additional metrics emitters, or audit reporters
that share the controller's lifecycle but are not part of the open-source
baseline.

## Out of scope

- Issuing the **first** security token. That still happens during
  `TryClaimSandbox` in `pkg/sandbox-manager/infra/sandboxcr/claim.go`. The two
  call sites share `pkg/identity/sandbox_token_helper.go` so semantics stay in
  sync.
- Cleanup on sandbox deletion / release. The sandbox controller owns lifecycle.

## Layout

```
pkg/controller/securitytokenrefresh/
├── token_refresh_controller.go   # Reconciler, Add(mgr), SetupHook registration
├── predicate.go                  # needsRefreshPredicate / isRefreshTarget
├── token_refresh_controller_test.go
├── predicate_test.go
└── core/
    ├── refresher.go              # Refresher interface + defaultRefresher
    └── refresher_test.go
```

## Flags

| Flag | Default | Notes |
| ---- | ------- | ----- |
| `--security-token-refresh-workers` | `10` | `MaxConcurrentReconciles` |
| `--security-token-refresh-lead-time` | `30m` | How long BEFORE expiration the refresh starts |
| `--security-token-refresh-jitter-ratio` | `0.1` | `±10%` jitter on the window |
| `--security-token-refresh-retry-after` | `1m` | Requeue interval after a failed refresh; with the default 30m lead window this allows ~30 retries before expiry |

## Conventions

- New `.go` files **must** start with the Apache 2.0 license header
  (`hack/boilerplate.go.txt`).
- All comments in English.
- Logging: `logf.FromContext(ctx)` (controller-runtime) or `klog` for static
  messages such as the Add() startup log.
- Tests are table-driven, use `expectError string` for error assertions, and
  inject fakes (`fakeRefresher`, `stubIdentityProvider`) instead of touching
  real network / global identity provider state at runtime.
- Tests that exercise `RegisterSetupHook` must snapshot and restore the
  package-level `setupHooks` slice (see `withSetupHooks(t)`) so they do not
  leak registrations into sibling tests.
- Do **not** reach into `pkg/sandbox-manager` from this package; shared logic
  belongs in `pkg/identity`.
