# AI Agent Guide (OpenKruise Agents)

## Role

Senior Go engineer specializing in Kubernetes operator development, familiar with controller-runtime, Kubebuilder, CRD
design, and cloud-native systems.

## Project Overview

OpenKruise Agents is a CNCF subproject managing AI agent sandbox workloads on Kubernetes, providing isolated
environments with resource pooling, hibernation/checkpoint, traffic routing, and E2B-compatible SDK.

Five Components:

- agent-sandbox-controller (`cmd/agent-sandbox-controller/`): Operator managing CRDs.
- sandbox-manager (`cmd/sandbox-manager/`): HTTP server with E2B-compatible REST APIs.
- sandbox-gateway (`cmd/sandbox-gateway/`): Envoy Go HTTP filter for traffic routing.
- agent-runtime (`cmd/agent-runtime/`): Sidecar running inside sandbox pods with envd-compatible APIs.
- traffic-extension (`cmd/traffic-extension/`): Envoy ext-proc gRPC server reconciling SecurityProfile CRDs and injecting tokens into egress traffic. See `docs/components/traffic-extension.md`.

## Tech Stack

- Language: Go
- Operator Framework: controller-runtime, Kubebuilder
- API Group: `agents.kruise.io/v1alpha1`
- Proxy: Envoy (ext_proc + Go HTTP filter)
- RPC: connectrpc (envd process communication)
- Linting: golangci-lint (gocyclo max 32)
- Testing: Ginkgo (E2E), pytest (E2B)
- Metrics: Prometheus

## Directory Structure

```
api/v1alpha1/      CRD types
cmd/               Entrypoints (controller, manager, gateway, runtime)
client/            Generated clientset (DO NOT edit)
config/            CRD, RBAC, manifests (generated)
pkg/
  controller/      Controllers (sandbox, set, claim, updateops)
  features/        Feature gates for controller (controller-only, MUST NOT be imported by sandbox-manager or servers)
  sandbox-manager/ Manager logic (infra, errors, logs, metrics). MUST NOT import pkg/features.
  servers/         E2B API, web framework. MUST NOT import pkg/features.
  proxy/           Envoy ext_proc gRPC server
  sandbox-gateway/ Envoy Go filter, route controller, registry
  traffic-extension/ Envoy ext-proc server for SecurityProfile (token injection)
  webhook/         Admission webhooks
  agent-runtime/   Runtime types, CSI providers, storages
  peers/           Peer discovery (memberlist)
  cache/           Cache layer with index, tasks, and controller-specific caches
  discovery/       Sandbox discovery service
  identity/        Identity and token providers (sandbox tokens, security tokens)
  utils/           Shared utilities (MUST NOT grow into a dumping ground; see Package Naming below)
proto/             Generated protobuf (DO NOT edit)
hack/              Scripts, boilerplate, certs
test/              E2E (Go), E2B (Python) tests
```

## CRD Types

- Sandbox (`sbx`): Single sandbox instance (Pod-backed). Supports pause/resume, checkpoint, in-place update.
- SandboxSet (`sbs`): Pool of idle Sandboxes (like ReplicaSet). Supports scale subresource.
- SandboxClaim (`sbc`): Claims sandboxes from SandboxSet (like PVC claiming PV). Supports batch, TTL, CSI mount.
- SandboxTemplate (`sbt`): Reusable pod spec template, referenced via `TemplateRef`.
- SandboxUpdateOps (`suo`): Batch update operations targeting sandboxes by label selector. Supports rolling/partitioned strategies.
- Checkpoint (`cp`): Checkpoint operation (memory/filesystem snapshot).
- SecurityProfile (`sp`): L7 egress traffic policy (match + actions like token injection); consumed by `traffic-extension`.

## Coding Conventions

### General Style

- Follow `Effective Go` and standard Go idioms.
- Every `.go` file must start with the Apache 2.0 license header (see `hack/boilerplate.go.txt`).
- Run `gofmt` and `goimports` on all code.
- Max cyclomatic complexity: 32 (enforced by golangci-lint).
- Use vendored dependencies (`go mod vendor`).

### Package Style

- Avoid generic package names like `utils`, `common`, `helpers`, `util`, or `base`. These names convey no meaning about
  what the package provides and tend to become dumping grounds for unrelated code.
- Every package name should describe its responsibility clearly (e.g., `expectations`, `inplaceupdate`, `timeout` instead
  of grouping them under a vague `utils` umbrella).
- When adding new functionality, prefer creating a purpose-specific sub-package under the appropriate domain directory
  over adding files to an existing generic package. For example, add `pkg/controller/rollback/` instead of
  `pkg/utils/rollback.go`.
- The existing `pkg/utils/` directory contains legacy code that should not grow further. New utilities must be placed in
  domain-specific packages. If a utility is only used by one component, keep it local to that component.
- If multiple packages share a helper, create a clearly-named package under `pkg/` that reflects its purpose
  (e.g., `pkg/identity/` for token providers, `pkg/discovery/` for sandbox discovery), not a catch-all `pkg/common/`.
- Avoid introducing dependency from utility package to domain-specific packages

### Error Handling

- Never ignore errors. Always check and handle `err`.
- Use `client.IgnoreNotFound(err)` for K8s Get/Update operations where not-found is acceptable.
- Use `errors.IsNotFound(err)` / `errors.IsAlreadyExists(err)` from `k8s.io/apimachinery/pkg/api/errors` for
  K8s-specific error checks.
- Use `errors.As()` / `errors.Is()` for error classification. Never use type assertions on errors.
- Define domain-specific error types with `ErrorCode` in `pkg/sandbox-manager/errors/` for the sandbox-manager layer.
- Never use `panic` for business errors. Only use `panic` during startup for unrecoverable initialization failures.

### Logging

- Controller layer: `logf.FromContext(ctx)` (controller-runtime/pkg/log)
- Manager layer: `klog.FromContext(ctx)` (k8s.io/klog/v2)
- Use structured logging (key-value pairs), never `fmt.Println`
- Add context via `.WithValues(key, value)`
- Debug logs: `.V(consts.DebugLogLevel)` (level 5)
- Context helpers: `logs.NewContext()` / `NewContextFrom()` / `Extend()`

### Testing

- Only run Go tests for packages under `pkg/` via `go test`.
- Never run any E2E test under the `test/` directory.
- Table-driven tests with descriptive `name` fields is a must: **ALWAYS** use table-driven tests for consistency and clarity.
- Reference test methods in same directory for best practices
- Use shared test helpers
- Target ≥80% unit test coverage
- Use `expectError string` instead of `expectError bool` to represent expected error state in test cases. An empty
  string means no error is expected; a non-empty string means an error is expected and the actual error message must
  contain that string (verified with `assert.Contains(t, err.Error(), tt.expectError)`).

### Multi-Agent Development Limits

- Core goal: accelerate development as much as possible while still delivering high-quality code.
- Sub-agents executing a specific task, including implementer and task reviewer agents, must not run all unit tests
  such as `go test ./pkg/...`.
- Implementer agents may run unit tests when necessary, such as during TDD or after implementation, but the test scope
  must stay focused on the changed behavior and must not include unnecessary packages.
- Reviewer agents must assume unit tests are already passing. They may run unit tests only when the code has an obvious
  issue that needs verification.
- Sub-agents must never run `go build`.
- The main agent, or the final global review agent, may run full package tests sparingly. `go build` must be reserved
  for final verification after the implementation is considered fully safe.

## Behavioral Rules

- Read related files before modifying code
- Don't edit `client/`, `proto/`, `config/crd/` — run `make generate` or `make manifests` instead
- Check `ctx.Done()` in retry/long-running operations
- Retry more before returning errors
- After modifying `api/v1alpha1/`, run `make generate manifests`
- Don't delete comments unless outdated
- New `.go` files need Apache 2.0 license header from `hack/boilerplate.go.txt`
- Use `Expectations` (`pkg/utils/expectations/`) for slow informer cache issues
- New APIs/architectural changes need proposal in `docs/proposals/`
- Ask user when unsure about business logic
- Always edit the files on your own, never use automation tools or scripts
- All comments must be in English
- When creating an `AGENTS.md` for a new submodule, also create a sibling `CLAUDE.md` in the same directory whose sole content is `@./AGENTS.md`
- Always commit with sign-off (e.g. `git commit -s`)
