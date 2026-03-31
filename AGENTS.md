# AI Agent Guide (OpenKruise Agents)

## Role

Senior Go engineer specializing in Kubernetes operator development, familiar with controller-runtime, Kubebuilder, CRD
design, and cloud-native systems.

## Project Overview

OpenKruise Agents is a CNCF subproject managing AI agent sandbox workloads on Kubernetes, providing isolated
environments with resource pooling, hibernation/checkpoint, traffic routing, and E2B-compatible SDK.

Four Components:

- agent-sandbox-controller (`cmd/agent-sandbox-controller/`): Operator managing CRDs.
- sandbox-manager (`cmd/sandbox-manager/`): HTTP server with E2B-compatible REST APIs.
- sandbox-gateway (`cmd/sandbox-gateway/`): Envoy Go HTTP filter for traffic routing.
- agent-runtime (`cmd/agent-runtime/`): Sidecar running inside sandbox pods with envd-compatible APIs.

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
  controller/      Controllers (sandbox, set, claim)
  sandbox-manager/ Manager logic (infra, errors, logs)
  servers/         E2B API, web framework
  proxy/           Envoy ext_proc gRPC server
  sandbox-gateway/ Envoy Go filter, route controller
  webhook/         Admission webhooks
  agent-runtime/   Runtime types, CSI providers
  peers/           Peer discovery (memberlist)
  utils/           Shared utilities
proto/             Generated protobuf (DO NOT edit)
hack/              Scripts, boilerplate, certs
test/              E2E (Go), E2B (Python) tests
```

## CRD Types

- Sandbox (`sbx`): Single sandbox instance (Pod-backed). Supports pause/resume, checkpoint, in-place update.
- SandboxSet (`sbs`): Pool of idle Sandboxes (like ReplicaSet). Supports scale subresource.
- SandboxClaim (`sbc`): Claims sandboxes from SandboxSet (like PVC claiming PV). Supports batch, TTL, CSI mount.
- SandboxTemplate (`sbt`): Reusable pod spec template, referenced via `TemplateRef`.
- Checkpoint (`cp`): Checkpoint operation (memory/filesystem snapshot).

## Coding Conventions

### General Style

- Follow `Effective Go` and standard Go idioms.
- Every `.go` file must start with the Apache 2.0 license header (see `hack/boilerplate.go.txt`).
- Run `gofmt` and `goimports` on all code.
- Max cyclomatic complexity: 32 (enforced by golangci-lint).
- Use vendored dependencies (`go mod vendor`).

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

- Table-driven tests with descriptive `name` fields
- Reference test methods in same directory for best practices
- Use shared test helpers
- Target ≥80% unit test coverage

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
