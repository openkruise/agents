---
title: Dynamic Environment Variable Injection for Pre-warmed Sandboxes
authors:
  - "@furykerry"
reviewers:
  - "@BH4AWS"
  - "@zmberg"
creation-date: 2026-07-01
status: provisional
---

# Dynamic Environment Variable Injection for Pre-warmed Sandboxes

This proposal introduces a mechanism to dynamically inject environment variables into pre-warmed sandbox pods
**after** allocation, enabling users to specify per-claim environment variables and custom commands that only
execute once the sandbox is assigned and initialized.

## Motivation

In the current architecture, a SandboxSet pre-creates a pool of warm sandbox pods. The pod's container command
is defined in the SandboxTemplate and starts executing **immediately** when the pod is created — long before any
user claims the sandbox. This means:

1. **No per-claim environment customization**: Environment variables injected via the envd `/init` endpoint
   after claim are not available to the container's main process, which has already started.
2. **No deferred command execution**: The user's command (defined in the template) runs at pod startup, not at
   claim time. There is no way to block execution until the sandbox is claimed and properly initialized.
3. **Race condition between init and command**: Even when `InitRuntime` sets env vars via the agent-runtime
   `/init` endpoint, the container's main process may have already read the environment at startup.
4. **Data loss on resume/upgrade**: When a sandbox is paused (pod deleted) or upgraded (pod recreated),
   the container's writable layer and any `emptyDir` volumes may get lost. Environment files written during
   the initial claim must be restored after the new pod starts.

### Goals

- Enable per-claim environment variable injection that is available to the user's command at execution time.
- Defer user command execution until the sandbox is claimed and environment is fully initialized.
- Restore environment variables automatically after sandbox resume (wake) or upgrade (pod recreation).
- Maintain backward compatibility with existing SandboxSet/SandboxClaim/E2B API flows.
- Support both SandboxClaim CRD and E2B sandbox create API.

### Non-Goals/Future Work

- Dynamic env var injection for already-running (post-claim) sandboxes.
- Secret/ConfigMap-based env var sources (future enhancement).
- Env var injection without agent-runtime (envd-less path).
- In-place upgrade support for env var changes on the Sandbox CR. Currently, modifying the
  container spec triggers in-place upgrade which does not support env var updates.

## Proposal

### Overview

The solution introduces a **deferred execution wrapper** — `run_with_envs.sh` — that replaces the user's
original container command. This wrapper blocks until an initialization signal file appears, then sources
the environment variables and executes the original command.

```
┌─────────────────────────────────────────────────────────────────┐
│  SandboxSet creates pre-warmed pod                              │
│                                                                 │
│  Container command: run_with_envs.sh -- <original-command>      │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ run_with_envs.sh                                         │  │
│  │  1. Parse args after "--" as original command            │  │
│  │  2. Wait for signal file: /var/sandbox/.env_ready        │  │
│  │     (waits indefinitely, no timeout)                     │  │
│  │  3. Source /var/sandbox/env.sh (env vars)                │  │
│  │  4. exec <original-command> with loaded environment      │  │
│  └───────────────────────────────────────────────────────────┘  │
│         ▲                                                       │
│         │ blocks until                                          │
│         │                                                       │
│  ┌──────┴─────────────────────────────────────────────────────┐ │
│  │ After claim OR after resume/upgrade:                        │ │
│  │  1. Write env vars to /var/sandbox/env.sh via /files API  │ │
│  │  2. Create signal file /var/sandbox/.env_ready             │ │
│  │                                                             │ │
│  │  envVars are persisted in Sandbox annotation                │ │
│  │  (AnnotationInitRuntimeRequest), surviving pod recreation   │ │
│  └────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

### User Stories

#### Story 1: E2B API with Custom Env Vars

As an E2B SDK user, I want to create a sandbox from a pre-warmed pool with custom environment variables,
and have my application start with those variables available.

```python
from e2b import Sandbox

sbx = Sandbox.create(
    template="python-pool",
    env_vars={
        "USER_ID": "user-123",
        "LOG_LEVEL": "debug",
    },
)
# The sandbox's main process starts with USER_ID and LOG_LEVEL available
```

#### Story 2: SandboxClaim CRD with Env Vars and Custom Command

As a platform developer, I want to claim a pre-warmed sandbox and inject env vars so that the sandbox's
command starts with the correct configuration.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: my-sandbox
spec:
  templateName: python-pool
  envVars:
    USER_ID: "user-123"
    LOG_LEVEL: "debug"
```

The SandboxSet template defines the command (no special configuration needed):

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: python-pool
spec:
  replicas: 5
  template:
    spec:
      containers:
        - name: main
          image: my-app:latest
          command: ["python", "app.py"]
```

When the claim provides `envVars` with `envVarsInjectionPolicy: Auto`, the claim
layer wraps the container command and the actual pod command becomes:

```
run_with_envs.sh -- python app.py
```

And `python app.py` only starts after `env.sh` is written with `USER_ID` and `LOG_LEVEL`.

> **Note**: Secrets (e.g., API keys, database passwords) should **not** be passed via `envVars`.
> Use the traffic-extension's SecurityProfile for dynamic secret injection via L7 traffic interception.

#### Story 3: No Env Vars — Immediate Execution

If no `envVars` are specified in the claim, the wrapper script should detect this and execute the original
command immediately without waiting, preserving the current fast-path behavior.

#### Story 4: EnvVarsInjectionPolicy=None — No Deferred Execution

As a platform developer, I want to claim a sandbox and persist env vars to the Sandbox CR without
blocking the container's command. The env vars are available on the Sandbox CR for other processes
(e.g., SDK, sidecars) to read, but the main container command starts immediately without waiting.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: my-sandbox
spec:
  templateName: python-pool
  envVarsInjectionPolicy: None
  envVars:
    USER_ID: "user-123"
```

With `None` policy, no command wrapping or signal file mechanism is used. The env vars are still
written to the Sandbox CR during claim, ensuring they survive resume and upgrade.

### Implementation Details

#### 1. Command Wrapping at Pod Creation Time

**Where**: Sandbox controller's pod generation logic (`pkg/controller/sandbox/core/pod_control.go`
`GeneratePodFromSandbox`).

**What**: When the Sandbox's `EnvVarsInjectionPolicy` is `Auto` (copied from the SandboxClaim
during claim), replace each container's `Command` with:

```yaml
command: ["/bin/bash", "/var/sandbox/run_with_envs.sh", "--"]
args: <original-command + original-args>
```

The original command and args are preserved as arguments after `--`, following the same convention already
used by `envd-run.sh`.

**Key design decisions**:
- The wrapper script path (`/var/sandbox/run_with_envs.sh`) is a well-known path, similar to `ENVD_DIR`.
- The original `Command` + `Args` are concatenated and passed as arguments after `--`.
- If the container has no `Command` (relies on image entrypoint), the wrapper is **not** injected,
  preserving backward compatibility.

#### 2. The `run_with_envs.sh` Wrapper Script

The script is included in the agent-runtime image and copied to a shared volume at pod startup.

```bash
#!/bin/bash
set -e

SIGNAL_FILE="${SANDBOX_ENV_DIR:-/var/sandbox}/.env_ready"
ENV_FILE="${SANDBOX_ENV_DIR:-/var/sandbox}/env.sh"

# Parse arguments: everything after "--" is the original command.
# When used by probes (startupProbe, livenessProbe, readinessProbe),
# --probe is passed to enable probe mode.
PROBE_MODE=false
USER_CMD=()
while [ $# -gt 0 ]; do
    case "$1" in
        --probe)
            PROBE_MODE=true
            shift
            ;;
        --)
            shift
            USER_CMD=("$@")
            shift $#
            ;;
        *)
            shift
            ;;
    esac
done

if [ ${#USER_CMD[@]} -eq 0 ]; then
    echo "[run_with_envs] No command specified, exiting."
    exit 0
fi

# If signal file does not exist yet (sandbox not claimed/initialized):
if [ ! -f "$SIGNAL_FILE" ]; then
    if [ "$PROBE_MODE" = "true" ]; then
        # Probe mode: return success before env initialization.
        # This keeps pre-warmed pods healthy while waiting for claim.
        echo "[run_with_envs] probe: env not ready, returning success."
        exit 0
    fi
    # Main command mode: wait for the signal file indefinitely.
    # The controller guarantees the signal file will be created after claim
    # (or after resume/upgrade re-initialization).
    echo "[run_with_envs] Waiting for environment initialization..."
    while [ ! -f "$SIGNAL_FILE" ]; do
        sleep 1
    done
fi

# Source environment variables
if [ -f "$ENV_FILE" ]; then
    echo "[run_with_envs] Sourcing environment from $ENV_FILE"
    set -a
    source "$ENV_FILE"
    set +a
fi

echo "[run_with_envs] Starting command: ${USER_CMD[*]}"
exec "${USER_CMD[@]}"
```

**Key behaviors**:
- **Two modes**: The script supports main-command mode (default) and probe mode (`--probe`).
  - **Main-command mode**: Blocks indefinitely until the signal file appears, then sources env
    vars and executes the command. Used for the container's main process.
  - **Probe mode (`--probe`)**: If the signal file does not exist yet, returns success
    immediately (`exit 0`). If the signal file exists, sources env vars and runs the probe
    command. This keeps pre-warmed pods healthy while waiting for claim.
- **Signal file (`/var/sandbox/.env_ready`)**: A simple empty file that indicates env vars are ready.
- **Env file (`/var/sandbox/env.sh`)**: Contains `export KEY=VALUE` lines for each env var.
- **No timeout**: In main-command mode, the script waits indefinitely. The controller guarantees
  the signal file will be created after claim, resume, or upgrade. If the sandbox is never claimed,
  it remains in the pool and the pod is eventually cleaned up by the SandboxSet's normal lifecycle.

#### 3. Environment Variable Injection After Claim

**Where**: After the sandbox is successfully claimed (in `modifyPickedSandbox` or the post-claim
initialization flow), the controller/sandbox-manager uses the agent-runtime to:

1. **Write the env file** via the envd `/files` API:

   Generate `/var/sandbox/env.sh` content:
   ```bash
   export USER_ID="user-123"
   export LOG_LEVEL="debug"
   ```

2. **Create the signal file** via the envd `/files` API:

   Write an empty file at `/var/sandbox/.env_ready`.

**Sequence**:

```
Claim completes
    │
    ▼
modifyPickedSandbox() writes annotations
    │
    ▼
WaitReady (sandbox pod Running + envd ready)
    │
    ▼
InitRuntime (existing /init endpoint for access token, etc.)
    │
    ▼
Write env.sh via /files API      ──┐
    │                              │ These two steps can be
    ▼                              │ done in a single call
Write .env_ready via /files API  ──┘
    │
    ▼
run_with_envs.sh unblocks → sources env.sh → exec user command
```

#### 4. EnvVarsInjectionPolicy on SandboxClaim

The injection policy is controlled by a **new field on SandboxClaim** (and the E2B create API extension):

```go
// EnvVarsInjectionPolicy defines how environment variables are injected into the sandbox.
// +enum
// +kubebuilder:validation:Enum=None;Auto
type EnvVarsInjectionPolicy string

const (
    // EnvVarsInjectionPolicyAuto: The controller wraps the container command with
    // run_with_envs.sh, writes env.sh and the signal file after claim (and after resume/upgrade).
    // The user's command starts only after env vars are available.
    EnvVarsInjectionPolicyAuto EnvVarsInjectionPolicy = "Auto"

    // EnvVarsInjectionPolicyNone(default): No command wrapping or deferred execution.
    // Env vars are still persisted to the Sandbox CR during claim, ensuring they survive
    // resume and upgrade. However, the container's command starts immediately without
    // waiting for env vars to be written to the filesystem.
    EnvVarsInjectionPolicyNone EnvVarsInjectionPolicy = "None"
)
```

**SandboxClaim API change**:

```go
type SandboxClaimSpec struct {
    // ...existing fields...

    // EnvVarsInjectionPolicy controls how environment variables are injected.
    // - Auto: wraps command with run_with_envs.sh, writes env.sh + signal file.
    // - None: persists envVars to the Sandbox CR but does not wrap the command.
    // In both cases, envVars are written to the Sandbox CR during claim.
    // +optional
    // +kubebuilder:default="None"
    // +kubebuilder:validation:Enum=None;Auto
    EnvVarsInjectionPolicy EnvVarsInjectionPolicy `json:"envVarsInjectionPolicy,omitempty"`
}
```

**E2B API extension**:

```go
type NewSandboxRequestExtension struct {
    // ...existing fields...
    EnvVarsInjectionPolicy EnvVarsInjectionPolicy // default: None
}
```

**Behavior per policy**:

| Behavior | `Auto` | `None`(default) |
|----------|--------------|-----------------|
| Env vars persisted to `AnnotationInitRuntimeRequest` | Yes | Yes             |
| Env vars written to Sandbox CR container spec | No | No              |
| Command wrapped with `run_with_envs.sh` | Yes | No              |
| env.sh + signal file written after claim | Yes | No              |
| env.sh + signal file written after resume/upgrade | Yes | No              |
| User command starts immediately | No (waits for env) | Yes             |

> **Important**: Env vars are **not** written to the Sandbox CR's container `env` spec during claim.
> Modifying the container spec would trigger the in-place upgrade path, which does not currently
> support env var changes. Env vars are only persisted to `AnnotationInitRuntimeRequest` (an
> annotation, not a spec field) and injected into the container filesystem via the `/files` API.
> In-place upgrade support for env vars is tracked as future work.

**How the policy is consumed**:

- **During claim**: The claim layer copies `EnvVarsInjectionPolicy` from the SandboxClaim to
  the Sandbox CR's spec. It also writes `envVars` to `AnnotationInitRuntimeRequest` regardless
  of the policy. This does **not** modify the container spec's `env` field (which would trigger
  in-place upgrade).
- **During pod creation**: The sandbox controller checks `Sandbox.Spec.EnvVarsInjectionPolicy`.
  If `Auto`, it wraps the command and exec probes with `run_with_envs.sh`.
- **During resume/upgrade**: The `Initialize` function reads `AnnotationInitRuntimeRequest` to
  recover env vars, and only calls `InjectEnvVars` when `EnvVarsInjectionPolicy` is `Auto`.

#### 5. Shared Volume for Signal/Env Files

A new `emptyDir` volume is mounted at `/var/sandbox/` in both the agent-runtime init container
and the main container:

```yaml
volumes:
  - name: sandbox-env
    emptyDir: {}

# Main container
containers:
  - name: main
    volumeMounts:
      - name: sandbox-env
        mountPath: /var/sandbox

# Agent-runtime init container (already exists)
initContainers:
  - name: runtime
    volumeMounts:
      - name: sandbox-env
        mountPath: /var/sandbox
```

The envd `/files` API writes to `/var/sandbox/env.sh` and `/var/sandbox/.env_ready` from within
the agent-runtime container's filesystem, which is shared with the main container via the emptyDir.

#### 5.1. Probes and Lifecycle Hooks

**Exec probes**: When `EnvVarsInjectionPolicy` is `Auto`, exec probes (`startupProbe`,
`livenessProbe`, `readinessProbe`) should also use `run_with_envs.sh` in **probe mode**
(`--probe`). In probe mode, the script returns success immediately if env vars are not yet
initialized, keeping the pre-warmed pod healthy while waiting for claim.

```yaml
startupProbe:
  exec:
    command: ["/bin/bash", "/var/sandbox/run_with_envs.sh", "--probe", "--", "/usr/local/bin/health-check"]
livenessProbe:
  exec:
    command: ["/bin/bash", "/var/sandbox/run_with_envs.sh", "--probe", "--", "/usr/local/bin/health-check"]
readinessProbe:
  exec:
    command: ["/bin/bash", "/var/sandbox/run_with_envs.sh", "--probe", "--", "/usr/local/bin/ready-check"]
```

**Probe behavior before and after claim**:

| Phase | Probe behavior | Rationale |
|-------|---------------|----------|
| Pre-warmed (before claim) | Returns success immediately | Pod must pass health checks while waiting in the pool |
| After claim + env ready | Sources env vars, runs actual probe command | Probe runs with correct environment |
| After resume/upgrade (before re-init) | Returns success immediately | Pod must pass health checks during re-initialization |

**Non-exec probes (Limitation)**: `httpGet` and `tcpSocket` probes are **not** wrapped and
cannot be modified to use `run_with_envs.sh`. They run as-is without per-claim env vars and
without the probe-mode early-success behavior. Users should prefer exec probes when per-claim
env vars or deferred initialization is required.

**PostStartHook (Limitation)**: The container's `postStartHook` is **not** wrapped with
`run_with_envs.sh`. The hook executes immediately after the container starts, before the env vars
are written by the controller. Users must not rely on per-claim env vars in `postStartHook`.

| Hook/Probe | Wrapped with `run_with_envs.sh` | Has per-claim env vars | Pre-init behavior |
|-----------|-------------------------------|------------------------|-------------------|
| `startupProbe` (exec) | Yes (probe mode) | Yes (after init) | Returns success |
| `livenessProbe` (exec) | Yes (probe mode) | Yes (after init) | Returns success |
| `readinessProbe` (exec) | Yes (probe mode) | Yes (after init) | Returns success |
| `startupProbe` (http/tcp) | **No** | **No** | Runs as-is |
| `livenessProbe` (http/tcp) | **No** | **No** | Runs as-is |
| `readinessProbe` (http/tcp) | **No** | **No** | Runs as-is |
| `postStartHook` | **No** | **No** | Runs as-is |
| `preStopHook` | No (runs at shutdown) | N/A | N/A |

#### 6. Integration with Existing InitRuntime Flow

The current `InitRuntime` flow sends a POST to `/init` with `envVars`. With this proposal:

- The `/init` endpoint continues to handle access token setup and other initialization.
- A **new step** is added after `/init`: write `env.sh` + signal file via the `/files` API.
- Alternatively, the `/init` endpoint in envd can be extended to handle env file writing natively,
  combining both operations into a single API call.

**Option A (Recommended)**: Add a new function `InjectEnvVars` in `pkg/utils/runtime/runtime.go` that:
1. Calls `/files` API to write `env.sh`.
2. Calls `/files` API to write `.env_ready`.

This keeps the change minimal and avoids modifying the envd binary.

**Option B (Future)**: Extend the envd `/init` endpoint to accept a `writeEnvFile` flag that
atomically writes the env file and signal file.

#### 7. Environment Restoration After Resume/Wake and Upgrade

When a sandbox is **paused** (pod deleted) or **upgraded** (pod recreated), the `emptyDir` volume
at `/var/sandbox/` is lost. The new pod starts fresh, and `run_with_envs.sh` blocks again waiting
for the signal file. The environment must be restored as part of the post-recreation initialization.

**Key design principle**: During claim, `envVars` are **always** written to the Sandbox's
`AnnotationInitRuntimeRequest` annotation, regardless of the `EnvVarsInjectionPolicy` (`Auto` or `None`).
This annotation lives on the Sandbox CR (not the pod) and survives pod deletion. This is the
foundation for environment restoration after any pod recreation event.

Note: The env vars are written to the **annotation** only — not to the container spec. Modifying
the container spec would trigger in-place upgrade, which does not currently support env var changes.

**Existing mechanism**: The `Initialize` function in `pkg/controller/sandbox/core/sandbox_initializer.go`
already handles post-recreation initialization for both resume and upgrade scenarios. It calls
`reinitRuntime`, which reads the `AnnotationInitRuntimeRequest` annotation (containing `envVars`)
from the Sandbox object and calls the envd `/init` endpoint.

**Proposed change**: Extend `Initialize` to also call `InjectEnvVars` after `reinitRuntime`
when the Sandbox's `EnvVarsInjectionPolicy` is `Auto`:

```
Initialize (after resume/upgrade)
    │
    ▼
reinitRuntime (existing: reads AnnotationInitRuntimeRequest, calls /init)
    │
    ▼
Check EnvVarsInjectionPolicy on Sandbox
    │
    ├── Auto: InjectEnvVars (writes env.sh + .env_ready via /files API)
    │       │
    │       ▼
    │   run_with_envs.sh unblocks → sources env.sh → exec user command
    │
    └── None: skip InjectEnvVars (user command already running, no blocking)
```

This ensures that after every pod recreation (resume from pause, recreate upgrade, or container
restart), the environment variables are restored and the user command starts with the correct
environment.

**Sequence for resume/wake**:

```
Sandbox paused (pod deleted, emptyDir lost)
    │
    ▼
Sandbox resumed (new pod created)
    │
    ▼
Pod Running + envd ready
    │
    ▼
EnsureSandboxResumed → sets RuntimeInitialized = Pending
    │
    ▼
EnsureSandboxUpdated → Initialize()
    │
    ├── reinitRuntime() → POST /init (access token, etc.)
    │
    └── If Auto: InjectEnvVars() → write env.sh + .env_ready via /files API
            │
            ▼
    run_with_envs.sh unblocks
```

**Sequence for upgrade (Recreate policy)**:

```
Sandbox upgrading (old pod deleted, new pod created)
    │
    ▼
New pod Running + envd ready
    │
    ▼
EnsureSandboxUpgraded → Initialize()
    │
    ├── reinitRuntime() → POST /init
    │
    └── If Auto: InjectEnvVars() → write env.sh + .env_ready
            │
            ▼
    run_with_envs.sh unblocks
```

### API Changes

#### New SandboxClaim Field: `EnvVarsInjectionPolicy`

```go
// EnvVarsInjectionPolicy defines how environment variables are injected into the sandbox.
type EnvVarsInjectionPolicy string

const (
    EnvVarsInjectionPolicyAuto EnvVarsInjectionPolicy = "Auto"
    EnvVarsInjectionPolicyNone EnvVarsInjectionPolicy = "None"
)
```

Added to `SandboxClaimSpec` with `+kubebuilder:default="None"`. Also exposed via the E2B
`NewSandboxRequestExtension`.

#### New Sandbox Spec Field: `EnvVarsInjectionPolicy`

The claim layer copies `EnvVarsInjectionPolicy` from `SandboxClaim` to the Sandbox CR's spec.
The sandbox controller reads this field directly during pod generation and initialization.

```go
type SandboxSpec struct {
    // ...existing fields...

    // EnvVarsInjectionPolicy controls how environment variables are injected.
    // Copied from SandboxClaim during claim.
    // +optional
    EnvVarsInjectionPolicy EnvVarsInjectionPolicy `json:"envVarsInjectionPolicy,omitempty"`
}
```

No separate annotation (e.g., `AnnotationDeferredEnv`) is needed — the CRD field is the
authoritative source.

#### Env Vars Persisted via Annotation (Not Container Spec)

Regardless of the injection policy (`Auto` or `None`), the claim layer **always writes `envVars`
to the Sandbox's `AnnotationInitRuntimeRequest` annotation**. The env vars are **not** written to
the container spec's `env` field, because that would trigger the in-place upgrade path which does
not currently support env var changes.

This design ensures:

- Env vars survive pod deletion during pause/resume and upgrade.
- The `Initialize` function can always read env vars from the annotation after pod recreation.
- For `Auto` policy: env vars are re-written to the filesystem (env.sh + signal file).
- For `None` policy: env vars remain in the annotation for `reinitRuntime` but are not pushed to
  the container filesystem.

### Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| `run_with_envs.sh` blocks indefinitely if claim never happens | The pre-warmed sandbox remains in the pool. The SandboxSet controller manages pool lifecycle (scale-down, cleanup). Unclaimed pods are deleted when the SandboxSet scales down or the sandbox TTL expires. |
| Env vars contain special characters breaking `env.sh` | Use proper shell escaping when generating `env.sh`. Alternatively, write env vars as a JSON file and parse with a helper. |
| Shared volume adds overhead | `emptyDir` is lightweight; the volume only holds small text files. |
| Backward compatibility with existing sandboxes | Controlled by `EnvVarsInjectionPolicy` field (default `None`, opt-in to `Auto`). Existing sandboxes created before this feature are unaffected. |
| Security: env vars visible in `/var/sandbox/env.sh` on disk | File permissions set to `0600`. Secrets should **not** be passed via `envVars`; use the traffic-extension's SecurityProfile for dynamic secret injection via L7 traffic interception. |
| Pod restart loses env.sh and signal file (emptyDir ephemeral) | **Handled by design**: The `Initialize` function in `sandbox_initializer.go` re-writes env.sh and signal file after every pod recreation (resume/wake, upgrade). The envVars are persisted in the `AnnotationInitRuntimeRequest` annotation on the Sandbox CR, which survives pod deletion. |

## Alternatives

### Alternative 1: Restart Container After Claim

After claim, kill the main process and restart it with the correct env vars. This is disruptive and
adds complexity in managing process lifecycle.

### Alternative 2: Use Kubernetes Downward API + ConfigMap

Create a ConfigMap per claim with env vars and mount it. This requires API server writes per claim
and has latency issues. It also doesn't solve the "block until ready" problem.

### Alternative 3: Extend envd `/init` to Set Process Environment

Modify the envd binary to set environment variables on the running process via `/proc/PID/environ`.
This is fragile, non-portable, and doesn't affect already-started processes.

### Alternative 4: Env vars via Container Env + Pod Recreation

Set env vars in the pod spec and recreate the pod. This defeats the purpose of pre-warming since the
pod must be recreated for each claim.

## Upgrade Strategy

- **New sandboxes**: The claim layer copies `EnvVarsInjectionPolicy` to the Sandbox CR and writes
  env vars to `AnnotationInitRuntimeRequest`. Command wrapping and filesystem injection are
  controlled by the `EnvVarsInjectionPolicy` field (`None` by default; `Auto` is opt-in).
- **Existing sandboxes**: No changes needed. Sandboxes without the field behave identically.
- **Agent-runtime image**: Must include `run_with_envs.sh` with both main-command and probe modes.
- **Controller**: Must check `Sandbox.Spec.EnvVarsInjectionPolicy` during pod generation and perform
  command + probe wrapping. Must call `InjectEnvVars` in the `Initialize` flow for resume/upgrade
  scenarios when the policy is `Auto`.
- **SandboxClaim CRD**: Requires a new `envVarsInjectionPolicy` field. Run `make generate manifests`
  after adding the field.
- **Sandbox CRD**: Requires a new `envVarsInjectionPolicy` spec field (copied from claim).
  Run `make generate manifests` after adding the field.

### Known Limitations

- **postStartHook**: The container's `postStartHook` is **not** wrapped with `run_with_envs.sh`
  and cannot access per-claim env vars. Users must not rely on per-claim env vars in postStartHook.
- **Non-exec probes**: `httpGet` and `tcpSocket` probes are **not** wrapped with `run_with_envs.sh`
  and do not benefit from probe-mode early-success behavior or per-claim env vars. Users should
  prefer exec probes when deferred initialization is required.
- **In-place upgrade**: Env vars are not written to the container spec to avoid triggering in-place
  upgrade, which does not support env var changes.

## Implementation History

- [ ] 07/01/2026: Proposed design for dynamic environment variable injection
