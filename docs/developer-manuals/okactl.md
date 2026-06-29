# okactl

`okactl` is a kubectl-style CLI tool for OpenKruise Agents that provides simplified operational commands for managing
sandbox resources. It wraps complex Kubernetes API operations behind user-friendly commands, targeting operators who may
not be familiar with Kubernetes internals.

All commands support resource short names: `sandboxset` → `sbs`, `sandboxupdateops` → `suo`, `sandbox` → `sbx`.

## Installation

### Build from source (requires Go 1.24+)

```bash
make build-okactl
```

The binary will be output to `bin/okactl`. You can move it to your `$PATH`:

```bash
mv bin/okactl /usr/local/bin/okactl
```

### Download pre-built binary

Alternatively, you can download a pre-built binary from [GitHub Releases](https://github.com/Liquorice-Ma/agents/releases):

```bash
curl -L -o /usr/local/bin/okactl https://github.com/Liquorice-Ma/agents/releases/download/v0.1.0/okactl
chmod +x /usr/local/bin/okactl
```

---

## Quick Start

`okactl` connects to the Kubernetes cluster via kubeconfig, just like `kubectl`. Ensure your cluster has
[OpenKruise Agents deployed](https://openkruise.io/kruiseagents/developer-manuals/contribution#deploy-to-kubernetes)
before using.

```bash
# Verify cluster connectivity (uses default kubeconfig)
okactl scale sbs my-pool --replicas=10 -n sandbox-system

# Or specify a custom kubeconfig / context
okactl scale sbs my-pool --replicas=10 --kubeconfig=/path/to/kubeconfig --context=my-cluster
```

Key Notes:

- kubeconfig resolution order: `--kubeconfig` flag → `KUBECONFIG` environment variable → `~/.kube/config`
- Namespace defaults to `default`; use `-n` to specify a different namespace
- All commands are stateless client-side API operations — no daemon or local state

Once connected, you can manage sandbox resources:

```bash
# Scale up a SandboxSet pool
okactl scale sbs my-pool --replicas=10 -n production

# Rolling update container images (and wait for completion)
okactl set image sbs my-pool app=nginx:1.25 --wait

# Or check update progress separately
okactl status sbs my-pool

# Batch update images for claimed sandboxes
okactl create suo -l app=my-app app=nginx:1.25

# Check SUO batch update progress
okactl status suo <name>

# Restart a misbehaving container (without pod restart)
okactl restart sbx my-sandbox -c app
```

---

## 1. Scale SandboxSet

Scale the replicas of a SandboxSet. Uses JSON Merge Patch — atomic, no conflict risk.

```bash
okactl scale sandboxset <name> --replicas=<N> [flags]
okactl scale sbs <name> --replicas=<N> [flags]
```

| Flag | Short | Required | Description |
|------|-------|----------|-------------|
| `--replicas` | | Yes | Target replica count |
| `--namespace` | `-n` | No | Target namespace (default: `default`) |

### Examples

```bash
okactl scale sandboxset my-pool --replicas=10 -n production
okactl scale sbs my-pool --replicas=0
```

---

## 2. Set Image

Update container images in a SandboxSet. Uses Get + Update with `retry.RetryOnConflict` for optimistic locking.

```bash
okactl set image sandboxset <name> <container=image> [container=image ...] [flags]
```

| Flag | Short | Required | Description |
|------|-------|----------|-------------|
| `--wait` | `-w` | No | Poll every 3s until all replicas are updated and available |
| `--timeout` | | No | Timeout for `--wait` (default: `5m`; `0` disables timeout) |
| `--namespace` | `-n` | No | Target namespace (default: `default`) |

> **Note**: Refuses to operate on SandboxSets using `TemplateRef`. Users should modify the referenced `SandboxTemplate`
> directly.

### Examples

```bash
# Update a single container
okactl set image sbs my-pool app=nginx:1.25

# Update multiple containers
okactl set image sandboxset my-pool app=nginx:1.25 sidecar=envoy:1.28 -n staging

# Update and wait for the rollout to complete
okactl set image sbs my-pool app=nginx:1.25 --wait

# Update with a custom timeout
okactl set image sbs my-pool app=nginx:1.25 --wait --timeout=10m
```

When `--wait` is used and the update appears stalled, the command automatically diagnoses potential issues by
checking sandbox and pod status (e.g., ImagePullBackOff, insufficient resources).

---

## 3. Status

Check the update progress of SandboxSet or SandboxUpdateOps resources. Performs a one-shot status check
with auto-diagnosis when the update appears stuck.

### 3.1 Status SandboxSet (`status sbs`)

```bash
okactl status sbs <name> [flags]
okactl status sandboxset <name> [flags]
```

| Flag | Short | Required | Description |
|------|-------|----------|-------------|
| `--namespace` | `-n` | No | Target namespace (default: `default`) |

**Output format**: `my-pool  Updating  1/3 updated  1/3 available`

When a rollout appears stuck, the command automatically diagnoses potential issues using a three-layer error source:
1. `sandbox.Status.Message` — controller-reported messages
2. Pod `conditions[PodScheduled]` — scheduling failures (e.g., insufficient cpu)
3. Pod `containerStatuses` — runtime issues (ImagePullBackOff, CrashLoopBackOff, etc.)

**Examples**:

```bash
# One-shot status check with auto-diagnosis
okactl status sbs my-pool -n staging
```

### 3.2 Status SandboxUpdateOps (`status suo`)

```bash
okactl status suo <name> [flags]
okactl status sandboxupdateops <name> [flags]
```

| Flag | Short | Required | Description |
|------|-------|----------|-------------|
| `--namespace` | `-n` | No | Target namespace (default: `default`) |

**Output format**: `suo-zk7h7  Updating   0/2 updated  1 updating  0 failed`

Returns an error if the SUO enters the `Failed` phase.

**Examples**:

```bash
# Check SUO progress
okactl status suo suo-zk7h7 -n production
```

---

## 4. Restart Containers

Restart specific containers in a running sandbox without killing the entire pod. Creates an OpenKruise
`ContainerRecreateRequest` (CRR) under the hood.

```bash
okactl restart sandbox <name> -c <container> [-c <container> ...] [--all] [--failure-policy=Fail|Ignore] [flags]
```

| Flag | Short | Required | Description |
|------|-------|----------|-------------|
| `--container` | `-c` | No | Container name(s) to restart (repeatable) |
| `--all` | | No | Restart all containers in the sandbox |
| `--failure-policy` | | No | Failure policy: `Fail` (stop on error, default) or `Ignore` (continue on error) |
| `--namespace` | `-n` | No | Target namespace (default: `default`) |

> **Note**: At least one of `-c` or `--all` must be specified. Running without either
> will print available container names and exit with an error. The two flags are
> mutually exclusive.

> **Prerequisites**:
> - The OpenKruise [ContainerRecreateRequest CRD](https://openkruise.io/docs/user-manuals/containerrecreaterequest)
>   must be installed.
> - The target sandbox must be in `Running` phase. Restarting containers in a Pending or
>   Failed sandbox has no effect and will return an error.

### Examples

```bash
# Restart a single container
okactl restart sandbox my-sandbox -c app -n production

# Restart multiple containers
okactl restart sbx my-sandbox -c app -c sidecar

# Restart all containers (explicit)
okactl restart sbx my-sandbox --all

# Restart all containers, continuing even if one fails
okactl restart sbx my-sandbox --all --failure-policy=Ignore
```

---

## 5. Create SandboxUpdateOps (SUO)

Create a `SandboxUpdateOps` to batch update images for claimed sandboxes (those managed by `SandboxClaim`).

```bash
okactl create suo -l <key=value> <container=image> [container=image ...] [flags]
```

| Flag | Short | Required | Description |
|------|-------|----------|-------------|
| `--selector` | `-l` | Yes | Label selector to match target sandboxes |
| `--namespace` | `-n` | No | Target namespace (default: `default`) |

### Validation

- The selector must match at least one existing sandbox (validated via server-side label filtering)
- The selector supports full Kubernetes label selector syntax, including `key=value`,
  `key!=value`, `key in (v1,v2)`, and `!key`
- Image arguments must be in `container=image` format

### Examples

```bash
# Update all sandboxes matching a label
okactl create suo -l app=my-app app=nginx:1.25 -n production

# Update multiple containers
okactl create suo -l app=my-app app=nginx:1.25 sidecar=envoy:1.28
```

---

## Global Flags

Applied via root command persistent flags, available to all subcommands:

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--kubeconfig` | | `~/.kube/config` | Path to kubeconfig file |
| `--namespace` | `-n` | `default` | Target namespace |
| `--context` | | | Kubeconfig context to use |

---

## Complete Workflow Example

```bash
# 1. Scale up the SandboxSet pool
okactl scale sbs code-interpreter --replicas=10 -n sandbox-system

# 2. Update container images and wait for the rollout to complete
okactl set image sbs code-interpreter app=my-registry/interpreter:v2.0 --wait -n sandbox-system

# 3. Batch update images for claimed sandboxes
okactl create suo -l agents.kruise.io/claim-name=my-claim \
    app=my-registry/interpreter:v2.0 -n sandbox-system

# 4. Check SUO batch update progress
okactl status suo <suo-name> -n sandbox-system

# 5. Restart a misbehaving container
okactl restart sbx code-interpreter-abc12 -c app -n sandbox-system
```

---

## Development

### Project Structure

```
cmd/okactl/
  main.go                # Cobra root command, command grouping, custom usage template with alias display

pkg/cli/
  options.go             # GlobalOptions (kubeconfig, namespace, context), client builders
  scale.go               # scale sandboxset implementation (alias: sbs)
  setimage.go            # set image implementation + shared status/diagnosis functions
  status.go              # status sbs (SandboxSet) and status suo (SandboxUpdateOps) commands
  restart.go             # restart sandbox implementation (creates CRR)
  create.go              # create suo implementation
  *_test.go              # Table-driven unit tests for each command
```

### Run Unit Tests

```bash
go test ./pkg/cli/... -v
```

### Run E2E Tests

The E2E tests require a Kubernetes cluster with agent-sandbox-controller deployed. See
[`.github/workflows/e2e-okactl.yaml`](../../.github/workflows/e2e-okactl.yaml) for the full CI setup.

```bash
make build-okactl
export KUBECONFIG=~/.kube/config
export OKACTL_BIN=$(pwd)/bin/okactl
make ginkgo
./bin/ginkgo -timeout 10m -v --fail-fast --focus='okactl CLI' test/e2e
```
