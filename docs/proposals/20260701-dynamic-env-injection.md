---
title: Dynamic Environment Variable Injection for Pre-warmed Sandboxes
authors:
  - "@furykerry"
  - "@Luckydog691"
reviewers:
  - "@BH4AWS"
  - "@zmberg"
creation-date: 2026-07-01
last-updated: 2026-10-30
status: provisional
---

# Dynamic Environment Variable Injection for Pre-warmed Sandboxes

This proposal introduces a mechanism to dynamically inject environment variables into pre-warmed sandbox pods
**after** allocation, enabling users to specify per-claim environment variables and custom commands that only
execute once the sandbox is assigned and initialized.

> **Revision note (2026-10-27)**: this version keeps the original design. The main changes are the
> policy placement (pool-level), the no-envVars behavior, the wrapped container scope, and the file
> locations and lifecycle details.
>
> **Review revision (2026-10-29)**: the script distribution is now expressed in terms of the
> agent-runtime image and its per-pod envd volume, startup ordering is made explicit, the admission
> prerequisites for `Deferred` pools are stated (agent-runtime, envd volume mount, probe shapes),
> and the claim directory is a fixed path rather than a configurable variable.
>
> **Review revision (2026-10-30)**: the pool field is renamed to `startupMode` with the values
> `Immediate` and `Deferred`, a command-less main container is no longer rejected (such a pool only
> admits claims that carry `spec.command`), and `SandboxClaim` gains a `command` override that the
> wrapper executes instead of the template command.

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
- Allow a claim to override the main process command, including for pools whose template relies on the
  image entrypoint.
- Restore environment variables automatically after sandbox resume (wake) or upgrade (pod recreation).
- Maintain backward compatibility with existing SandboxSet/SandboxClaim/E2B API flows.
- Support both SandboxClaim CRD and E2B sandbox create API.

### Non-Goals/Future Work

- Dynamic env var injection for already-running (post-claim) sandboxes.
- Secret/ConfigMap-based env var sources (future enhancement).
- Env var injection without the agent-runtime (envd-less path). The wrapper is distributed with the
  agent-runtime image and staged into its envd volume, so a `Deferred` pool must declare the
  agent-runtime; admission rejects the combination because there is no distribution or delivery path.
- Command override through the E2B create API. The upstream E2B create call has no command field, and
  this proposal does not add one; the override is CRD-only.
- In-place upgrade support for env var changes on the Sandbox CR. Currently, modifying the
  container spec triggers in-place upgrade which does not support env var updates.

## Proposal

### Overview

The solution introduces a **deferred execution wrapper** — `run_with_envs.sh` — that replaces the user's
original container command. This wrapper blocks until an initialization signal file appears, then sources
the environment variables and executes the claim-supplied command if present, or the original command
otherwise.

```
┌─────────────────────────────────────────────────────────────────┐
│  SandboxSet creates pre-warmed pod                              │
│                                                                 │
│  Container command: run_with_envs.sh -- <template-command>      │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ run_with_envs.sh                                          │  │
│  │  1. Parse args after "--" as the template command         │  │
│  │  2. Wait for signal file (no timeout):                    │  │
│  │     /mnt/envd-claim/.env_ready                            │  │
│  │  3. Source /mnt/envd-claim/env.sh                         │  │
│  │  4. If /mnt/envd-claim/cmd.sh exists, exec it             │  │
│  │     (claim command); else exec the template command       │  │
│  └───────────────────────────────────────────────────────────┘  │
│         ▲                                                       │
│         │ blocks until                                          │
│         │                                                       │
│  ┌──────┴────────────────────────────────────────────────────┐  │
│  │ After claim OR after resume/upgrade:                      │  │
│  │  1. Write /mnt/envd-claim/env.sh via /files API           │  │
│  │  2. Write /mnt/envd-claim/cmd.sh if the claim sets        │  │
│  │     spec.command                                          │  │
│  │  3. Create signal file /mnt/envd-claim/.env_ready         │  │
│  │                                                           │  │
│  │ envVars and the claim command are persisted in the        │  │
│  │ Sandbox annotation, surviving pod recreation              │  │
│  └───────────────────────────────────────────────────────────┘  │
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

As a platform developer, I want to claim a pre-warmed sandbox, inject env vars, and optionally use my own
command, so that the sandbox's main process starts with the correct configuration.

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
  # Optional: overrides the template command for this claim only.
  command: ["python", "worker.py"]
```

The SandboxSet template defines the default command (no special configuration needed):

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: python-pool
spec:
  replicas: 5
  startupMode: Deferred
  template:
    spec:
      containers:
        - name: main
          image: my-app:latest
          command: ["python", "app.py"]
```

When the pool declares `startupMode: Deferred`, the controller wraps the main container command at pod
creation and the actual pod command becomes:

```
run_with_envs.sh -- python app.py
```

And `python worker.py` (the claim command) or `python app.py` (the template default) only starts after
`env.sh` is written with `USER_ID` and `LOG_LEVEL`.

> **Note**: Secrets (e.g., API keys, database passwords) should **not** be passed via `envVars`.
> Use the traffic-extension's SecurityProfile for dynamic secret injection via L7 traffic interception.

`Deferred` also requires the pool to declare the agent-runtime sidecar and mount its envd volume at
`/mnt/envd` in the main container. Admission rejects a `Deferred` pool that does not satisfy these;
see 1.1 for the staging and startup-ordering contract.

#### Story 3: No Env Vars — Deferred Start

If no `envVars` are specified in the claim, a `Deferred` pool still holds the main process until the claim
writes the readiness signal, then starts the original command with the pool's original environment. The
wrapper cannot execute immediately: at pod creation the controller does not know whether a later claim will
carry env vars, and a process environment cannot be changed after exec.

#### Story 4: Command-Less Template — the Claim Supplies the Command

As a platform developer, I package an application whose image defines `ENTRYPOINT`/`CMD` and I do not want
to duplicate the argv in the template. I set `startupMode: Deferred` and let every claim supply the command.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: app-pool
spec:
  startupMode: Deferred
  template:
    spec:
      containers:
        - name: main
          image: my-app:latest   # no command; the image entrypoint is not used
          volumeMounts:
            - name: envd-volume
              mountPath: /mnt/envd
```

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: my-sandbox
spec:
  templateName: app-pool
  envVars:
    USER_ID: "user-123"
  command: ["python", "worker.py"]
```

Admission does not reject the command-less pool. The controller wraps the container with an empty
template argv (`run_with_envs.sh --`) and the claim command is the only executable. A claim that omits
`command` against such a pool fails with reason `InvalidClaimSpec`: once the command is replaced, the
controller cannot recover the image entrypoint, so there is nothing to run.

#### Story 5: StartupMode=Immediate — No Deferred Execution

As a platform developer, I want to claim a sandbox and persist env vars to the Sandbox CR without
blocking the container's command. The env vars are available on the Sandbox CR for other processes
(e.g., SDK, sidecars) to read, but the main container command starts immediately without waiting.
The pool uses the default `startupMode: Immediate`.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxClaim
metadata:
  name: my-sandbox
spec:
  templateName: python-pool
  envVars:
    USER_ID: "user-123"
```

With `Immediate`, no command wrapping or signal file mechanism is used. The env vars are still
written to the Sandbox CR during claim (existing `/init` behavior), ensuring they survive resume and upgrade.
The main process starts immediately and does not see the claim env vars. A claim that sets `spec.command`
against an `Immediate` pool fails validation instead of being silently ignored.

### Implementation Details

#### 1. Command Wrapping at Pod Creation Time

**Where**: Sandbox controller's pod generation logic (`pkg/controller/sandbox/core/pod_control.go`
`GeneratePodFromSandbox`).

**What**: When the Sandbox's `StartupMode` is `Deferred` (declared on the pool and copied to the
Sandbox when it is created), replace the main container's `Command` with:

```yaml
command: ["/bin/bash", "/mnt/envd/run_with_envs.sh", "--"]
args: <template-command + template-args>   # omitted when the template declares none
```

The template command and args are preserved as arguments after `--`, following the same convention already
used by `envd-run.sh`.

**Key design decisions**:
- The wrapper script path (`/mnt/envd/run_with_envs.sh`) is a well-known path, shared with the envd payload.
- The template `Command` + `Args` are concatenated and passed as arguments after `--`. When the template
  declares neither, `args` is empty and the wrapper relies on the claim command.
- Only the main container (`containers[0]`) is wrapped; sidecars and init containers are never wrapped.
- A `Deferred` pool's main container does not need an explicit `Command`. Admission admits command-less
  pools; such a pool can only serve claims that set `spec.command` (see 3.1 and 4). The controller
  cannot reconstruct an image entrypoint once the command is replaced, so there is no silent fallback
  to the image default.
- Startup argv precedence: claim `command` > template `command` + `args` > none (claim rejected).
  `Immediate` pools are unaffected.
- On a command-less pool the wrapped `args` stays empty, and container runtimes fill an empty `args`
  with the image CMD. The wrapper never uses it: the claim command takes precedence, and a claim
  without `command` is rejected on such a pool.

#### 1.1. Script Distribution and Startup Ordering

The wrapper is neither provisioned by the node nor baked into the user's image. It ships in the
agent-runtime image and is staged into the per-pod envd `emptyDir` at `/mnt/envd`, which the pool
template mounts in both the main container and the runtime sidecar.

Two properties must hold before the main container executes its command:

1. **The envd volume is mounted at `/mnt/envd` in the main container.** Admission requires this for
   every `Deferred` pool: the controller cannot rely on a volume the template forgot to declare.
2. **`run_with_envs.sh` is present at `/mnt/envd/run_with_envs.sh`.** A sidecar (`initContainer`
   with `restartPolicy: Always`) is started before the main containers, but its entrypoint staging
   is not guaranteed to have completed by then. To make this a guarantee rather than a timing
   assumption, the controller injects an ordinary init container (`envd-scripts`) that uses the
   sidecar's agent-runtime image, mounts the envd volume, copies `/workspace/run_with_envs.sh` into
   `/mnt/envd/`, and exits. Ordinary init containers complete before any main container starts.

The injected container looks like this:

```yaml
initContainers:
  # Injected by the controller for Deferred pools.
  # Ordinary init container: no restartPolicy, so kubelet waits for it
  # to complete before starting the main containers.
  - name: envd-scripts
    image: <same image as the agent-runtime sidecar>
    command:
      - sh
      - -c
      - cp /workspace/run_with_envs.sh /mnt/envd/ && chmod 0755 /mnt/envd/run_with_envs.sh
    volumeMounts:
      - name: envd-volume
        mountPath: /mnt/envd
```

Unlike the agent-runtime sidecar, `envd-scripts` has no `restartPolicy: Always`; kubelet requires it
to terminate successfully before any main container starts. The controller injects it once
(idempotent by container name) and leaves its resource requests empty so it does not change Pod
scheduling.

The agent-runtime entrypoint keeps staging `envd` and `envd-run.sh` as it does today; the wrapper is
the only artifact the dedicated init container owns. Relying on the sidecar staging alone is
possible but leaves a first-start window where the main process exits with "no such file" and is
restarted by kubelet; the init container removes that window at the cost of one short-lived
container per pod.

#### 2. The `run_with_envs.sh` Wrapper Script

The script is included in the agent-runtime image and staged into the per-pod envd volume at pod
startup (see 1.1).

```bash
#!/bin/bash
set -e

SIGNAL_FILE="/mnt/envd-claim/.env_ready"
ENV_FILE="/mnt/envd-claim/env.sh"
CMD_FILE="/mnt/envd-claim/cmd.sh"
POD_UID="${SANDBOX_POD_UID:-}"

signal_ready() {
    [ -f "$SIGNAL_FILE" ] || return 1
    [ -z "$POD_UID" ] || [ "$(cat "$SIGNAL_FILE")" = "$POD_UID" ]
}

# Parse arguments: everything after "--" is the template command.
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
            echo "[run_with_envs] Error: unknown argument '$1'" >&2
            exit 1
            ;;
    esac
done

# If the signal for this pod generation is not ready (sandbox not claimed/initialized):
if ! signal_ready; then
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
    while ! signal_ready; do
        sleep 1
    done
fi

# Source environment variables
if [ -f "$ENV_FILE" ]; then
    echo "[run_with_envs] Sourcing environment from $ENV_FILE"
    set -a
    if ! source "$ENV_FILE"; then
        echo "[run_with_envs] Failed to source $ENV_FILE" >&2
        exit 1
    fi
    set +a
fi

# Claim command overrides the template command in main-command mode.
# Probe mode always runs its own probe command.
if [ "$PROBE_MODE" = "false" ] && [ -f "$CMD_FILE" ]; then
    echo "[run_with_envs] Executing claim command from $CMD_FILE"
    if ! source "$CMD_FILE"; then
        echo "[run_with_envs] Failed to source $CMD_FILE" >&2
        exit 1
    fi
fi

if [ ${#USER_CMD[@]} -eq 0 ]; then
    echo "[run_with_envs] Error: no command to run: the claim does not set spec.command and the template declares none" >&2
    exit 1
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
- **Signal file (`/mnt/envd-claim/.env_ready`)**: Contains the pod UID. The wrapper ignores a signal
  that does not match its own `SANDBOX_POD_UID` (generation check); in the per-pod emptyDir it always
  matches.
- **Env file (`/mnt/envd-claim/env.sh`)**: Contains `export KEY='value'` lines for each env var.
- **Claim command file (`/mnt/envd-claim/cmd.sh`)**: Written only when the claim sets `spec.command`.
  It contains a single `exec 'arg0' 'arg1' ...` line produced with the same single-quote escaping as
  `env.sh`, and is sourced after `env.sh` so the claim command inherits the claim environment. It is
  only consulted in main-command mode: probe invocations keep running their probe command. The
  wrapper never falls back from a claim command to the template command.
- **Claim directory (`/mnt/envd-claim`)**: a fixed path in this proposal, matching the
  controller-injected `sandbox-env-claim` volume; it is not configurable per pool or per claim.
  `SANDBOX_POD_UID` remains a reserved name: claim `envVars` already reject the `SANDBOX_` prefix,
  and pool templates must not set it, otherwise the generation check would reject every readiness
  signal.
- **Failure handling**: if `source env.sh` or `source cmd.sh` fails, the wrapper exits non-zero
  instead of exec'ing the command with a partial environment or argv.
- **No timeout**: In main-command mode, the script waits indefinitely. The controller guarantees
  the signal file will be created after claim or pod recreation (resume/upgrade). If the sandbox is
  never claimed, it remains in the pool and the pod is eventually cleaned up by the SandboxSet's normal
  lifecycle.

#### 3. Environment Variable Injection After Claim

**Where**: After the sandbox is successfully claimed (in `modifyPickedSandbox` or the post-claim
initialization flow), the controller/sandbox-manager uses the agent-runtime to:

1. **Write the env file** via the envd `/files` API:

   Generate `/mnt/envd-claim/env.sh` content:
   ```bash
   export USER_ID="user-123"
   export LOG_LEVEL="debug"
   ```

2. **Write the claim command file** via the envd `/files` API, when the claim sets `spec.command`:

   Generate `/mnt/envd-claim/cmd.sh` content:
   ```bash
   exec 'python' 'worker.py'
   ```

   The controller shell-quotes each argv element the same way it quotes `env.sh` values.

3. **Create the signal file** via the envd `/files` API:

   Write the pod UID to `/mnt/envd-claim/.env_ready`.

The order matters: `env.sh` and `cmd.sh` are written before the signal file, so the wrapper never
starts a command with incomplete data.

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
Write env.sh via /files API            ──┐
    │                                    │ these steps can be
    ▼                                    │ done in a single call
Write cmd.sh (if claim command set)      │
    │                                    │
    ▼                                    │
Write .env_ready via /files API        ──┘
    │
    ▼
run_with_envs.sh unblocks → sources env.sh → exec claim command or template command
```

If validation or delivery fails, the claim fails; do not return a claimed sandbox with a blocked main process.

#### 3.1. Claim-Level Command Override

A claim may replace the main process argv for its own sandbox. Because the wrapper has not exec'd yet,
the override is delivered as data, not as a pod spec change, so no pod recreation is needed.

```go
type SandboxClaimSpec struct {
    // ...existing fields...

    // Command overrides the main container command for the sandboxes claimed by
    // this SandboxClaim. The value is the complete argv and is executed directly
    // without a shell. It takes precedence over the wrapped template command.
    // Only valid when the target SandboxSet uses startupMode: Deferred.
    // This field is immutable once set.
    // +optional
    // +kubebuilder:validation:MinItems=1
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="command is immutable"
    Command []string `json:"command,omitempty"`
}
```

**Semantics**:

- `command` is the full argv (`argv[0]` plus arguments); it is exec'd as-is with no shell parsing and
  no merging with template `command`/`args`.
- Only `containers[0]` is affected; sidecars and init containers are untouched.
- The override is per claim, not per pool: each claimed sandbox executes its own claim command.
- `command` requires a `Deferred` pool. A claim that sets `command` against an `Immediate` pool
  completes with reason `InvalidClaimSpec`; ignoring the field would silently run the wrong process.
- A claim that omits `command` keeps the template command. If the template declares no command either,
  the claim completes with reason `InvalidClaimSpec` (see Story 4).
- The claim command is persisted in the Sandbox annotation together with the init request, so
  resume/wake and recreate upgrades replay the claim command rather than the template command.

**Delivery**: the controller writes `/mnt/envd-claim/cmd.sh` with the claim command and the wrapper
sources it after `env.sh`, so the command inherits the per-claim environment. The claim command is
delivered by the same `/files` call sequence as `env.sh`; there is no additional round trip.

**Probes**: wrapped exec probes still run the template probe commands. A claim command must therefore
keep whatever ports and health endpoints the template probes target; the controller cannot validate
this. `postStartHook` remains unwrapped and still runs before the claim command.

**Security**: a claim command lets a caller start an arbitrary process inside the pool container
without `pods/exec` permission. The capability is bounded by the pool's image, mounts, and network
policy, and it is granted by the existing `sandboxclaims` write permission. This proposal documents
the surface and does not add a separate gate; deployments that do not want it should not grant
`SandboxClaim` creation to untrusted callers.

#### 4. StartupMode on the pool

The startup mode is declared on the pool (`SandboxSet`) and copied to the Sandbox when it is created.
Warm-pool pods are created before a claim, and their command cannot be rewritten at claim time, so the
`Deferred`/`Immediate` decision must come from the pool. The claim can override the argv (3.1) but not
the mode.

Changing `startupMode` on an existing pool only affects Sandboxes created afterwards. Pods that already
exist keep their wrapping (or their absence) until they are recreated, so switching a pool between
modes should be done by rolling the pool, not by editing the field in place.

```go
// StartupMode defines when the sandbox's main process starts.
// +enum
// +kubebuilder:validation:Enum=Immediate;Deferred
type StartupMode string

const (
    // StartupModeDeferred: The controller wraps the main container command with
    // run_with_envs.sh, and delivers env.sh, the claim command, and the signal file
    // after claim (and after resume/upgrade). The main process starts only after
    // that delivery, with the claim command or the template command.
    StartupModeDeferred StartupMode = "Deferred"

    // StartupModeImmediate(default): No command wrapping or deferred execution.
    // Env vars are still persisted to the Sandbox CR during claim, ensuring they survive
    // resume and upgrade. However, the container's command starts immediately without
    // waiting for env vars to be written to the filesystem.
    StartupModeImmediate StartupMode = "Immediate"
)
```

**SandboxSet API change**:

```go
type SandboxSetSpec struct {
    // ...existing fields...

    // StartupMode controls when the sandbox's main process starts.
    // - Deferred: wraps the main container command with run_with_envs.sh and starts it
    //   only after the claim delivers env vars and the readiness signal. Requires the
    //   agent-runtime and its envd volume mounted at /mnt/envd. The main container may
    //   omit command; such a pool only admits claims that set spec.command.
    // - Immediate: no wrapping; envVars keep using the existing /init path and the main
    //   process starts with the container.
    // +optional
    // +kubebuilder:default="Immediate"
    // +kubebuilder:validation:Enum=Immediate;Deferred
    StartupMode StartupMode `json:"startupMode,omitempty"`
}
```

The `SandboxSpec` field below is copied from the pool when the Sandbox is created.

**E2B API**: no new field. The create call only carries `env_vars`; the pool declares the mode.

**Behavior per startup mode**:

| Behavior | `Deferred` | `Immediate`(default) |
|----------|------------|----------------------|
| Env vars persisted to `AnnotationInitRuntimeRequest` | Yes | Yes |
| Env vars written to Sandbox CR container spec | No | No |
| Command wrapped with `run_with_envs.sh` | Yes | No |
| Claim `command` applied | Yes | No (claim rejected) |
| env.sh + cmd.sh + signal file written after claim | Yes | No |
| env.sh + cmd.sh + signal file written after resume/upgrade | Yes | No |
| Main process starts immediately | No (waits for claim signal) | Yes |

> **Important**: Env vars are **not** written to the Sandbox CR's container `env` spec during claim.
> Modifying the container spec would trigger the in-place upgrade path, which does not currently
> support env var changes. Env vars are only persisted to `AnnotationInitRuntimeRequest` (an
> annotation, not a spec field) and injected into the container filesystem via the `/files` API.
> In-place upgrade support for env vars is tracked as future work.

**How the startup mode is consumed**:

- **Admission**: a `Deferred` pool must declare the agent-runtime, mount its envd volume at
  `/mnt/envd` in the main container, and avoid non-exec probes on application ports. An explicit
  main-container `Command` is not required. Claims cannot change the mode or the claim directory.
- **Claim validation**: a claim fails with `InvalidClaimSpec` when it sets `spec.command` against an
  `Immediate` pool, or when it omits `spec.command` against a `Deferred` pool whose template main
  container declares no `command`.
- **During Sandbox creation**: The SandboxSet controller copies `StartupMode` to the
  Sandbox CR's spec. The claim carries `envVars` and the optional `command`, written to
  `AnnotationInitRuntimeRequest` (existing `/init` behavior); it does not change the mode.
- **During pod creation**: The sandbox controller checks `Sandbox.Spec.StartupMode`.
  If `Deferred`, it wraps the main container command and exec probes with `run_with_envs.sh`.
- **During resume/upgrade**: The `Initialize` function reads `AnnotationInitRuntimeRequest` to
  recover env vars and the claim command, and only calls `InjectStartupData` when `StartupMode` is
  `Deferred` and the sandbox is already claimed.

#### 5. Shared Volume for Signal/Env Files

A new `emptyDir` volume is mounted at `/mnt/envd-claim/` in both the agent-runtime init container
and the main container:

```yaml
volumes:
  - name: sandbox-env-claim
    emptyDir: {}

# Main container
containers:
  - name: main
    volumeMounts:
      - name: sandbox-env-claim
        mountPath: /mnt/envd-claim

# Agent-runtime init container (already exists)
initContainers:
  - name: runtime
    volumeMounts:
      - name: sandbox-env-claim
        mountPath: /mnt/envd-claim
```

The envd `/files` API writes `/mnt/envd-claim/env.sh`, `/mnt/envd-claim/cmd.sh` (only when the claim
sets a command), and `/mnt/envd-claim/.env_ready` from within the agent-runtime container's
filesystem, which is shared with the main container via the emptyDir.

The claim volume is deliberately controller-owned instead of reusing the template-declared envd
volume: the controller guarantees it whenever `startupMode` is `Deferred`, so delivery never depends on a
volume the pool author may have forgotten to declare. The envd volume holds runtime payload (`envd`,
`envd-run.sh`, `run_with_envs.sh`); the claim volume holds only per-claim data.

#### 5.1. Probes and Lifecycle Hooks

**Exec probes**: When `StartupMode` is `Deferred`, exec probes (`startupProbe`,
`livenessProbe`, `readinessProbe`) should also use `run_with_envs.sh` in **probe mode**
(`--probe`). In probe mode, the script returns success immediately if env vars are not yet
initialized, keeping the pre-warmed pod healthy while waiting for claim.

```yaml
startupProbe:
  exec:
    command: ["/bin/bash", "/mnt/envd/run_with_envs.sh", "--probe", "--", "/usr/local/bin/health-check"]
livenessProbe:
  exec:
    command: ["/bin/bash", "/mnt/envd/run_with_envs.sh", "--probe", "--", "/usr/local/bin/health-check"]
readinessProbe:
  exec:
    command: ["/bin/bash", "/mnt/envd/run_with_envs.sh", "--probe", "--", "/usr/local/bin/ready-check"]
```

**Probe behavior before and after claim**:

| Phase | Probe behavior | Rationale |
|-------|---------------|----------|
| Pre-warmed (before claim) | Returns success immediately | Pod must pass health checks while waiting in the pool |
| After claim + env ready | Sources env vars, runs actual probe command | Probe runs with correct environment |
| After resume/upgrade (before re-init) | Returns success immediately | Pod must pass health checks during re-initialization |

**Non-exec probes (Limitation)**: `httpGet` and `tcpSocket` probes are **not** wrapped and
cannot be modified to use `run_with_envs.sh`. They run as-is without per-claim env vars and
without the probe-mode early-success behavior. For `Deferred` pools, admission should reject non-exec
probes that target application ports: during the warm period the main process is not running, so
such probes would fail and can crashloop the pool. Non-exec probes targeting the runtime's own
ports are allowed. Users should prefer exec probes when per-claim env vars or deferred
initialization is required. When a claim overrides the command (3.1), the wrapped probes still run
the template probe commands, so the claim command must keep the probed ports and endpoints alive.

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
- A **new step** is added after `/init`: write `env.sh`, the optional `cmd.sh`, and the signal file
  via the `/files` API.
- Alternatively, the `/init` endpoint in envd can be extended to handle env file writing natively,
  combining both operations into a single API call.

**Option A (Recommended)**: Add a new function `InjectStartupData` in `pkg/utils/runtime/runtime.go` that:
1. Calls `/files` API to write `env.sh`.
2. Calls `/files` API to write `cmd.sh` when the claim sets `command`.
3. Calls `/files` API to write `.env_ready` last.

This keeps the change minimal and avoids modifying the envd binary.

**Option B (Future)**: Extend the envd `/init` endpoint to accept a `writeEnvFile` flag that
atomically writes the env file, the claim command, and the signal file.

#### 7. Environment Restoration After Resume/Wake and Upgrade

When a sandbox is **paused** (pod deleted) or **upgraded** (pod recreated), the `emptyDir` volume
at `/mnt/envd-claim/` is lost. The new pod starts fresh, and `run_with_envs.sh` blocks again waiting
for the signal file. The environment must be restored as part of the post-recreation initialization.

**Key design principle**: During claim, `envVars` and the optional claim `command` are **always** written
to the Sandbox's `AnnotationInitRuntimeRequest` annotation, regardless of the `StartupMode` (`Deferred`
or `Immediate`). This annotation lives on the Sandbox CR (not the pod) and survives pod deletion. This is
the foundation for environment and command restoration after any pod recreation event.

Note: The env vars and the claim command are written to the **annotation** only — not to the container
spec. Modifying the container spec would trigger in-place upgrade, which does not currently support env
var changes.

**Existing mechanism**: The `Initialize` function in `pkg/controller/sandbox/core/sandbox_initializer.go`
already handles post-recreation initialization for both resume and upgrade scenarios. It calls
`reinitRuntime`, which reads the `AnnotationInitRuntimeRequest` annotation (containing `envVars`)
from the Sandbox object and calls the envd `/init` endpoint.

**Proposed change**: Extend `Initialize` to also call `InjectStartupData` after `reinitRuntime`
when the Sandbox's `StartupMode` is `Deferred`. Only replay for a sandbox that is already
claimed: an unclaimed warm-pool member recreated by a rolling update must keep waiting, because
replaying would start the main process before claim. The replayed command is the claim command when
one was recorded, otherwise the template command.

```
Initialize (after resume/upgrade)
    │
    ▼
reinitRuntime (existing: reads AnnotationInitRuntimeRequest, calls /init)
    │
    ▼
Check StartupMode on Sandbox
    │
    ├── Deferred: InjectStartupData (writes env.sh, cmd.sh, .env_ready via /files API)
    │       │
    │       ▼
    │   run_with_envs.sh unblocks → sources env.sh → exec claim command or template command
    │
    └── Immediate: skip InjectStartupData (main process already running, no blocking)
```

This ensures that after every pod recreation (resume from pause or recreate upgrade), the
environment variables and the claim command are restored and the main process starts with the correct
argv and environment. A container restart keeps the `emptyDir` and does not need a replay.

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
    └── If Deferred: InjectStartupData() → write env.sh, cmd.sh, .env_ready via /files API
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
    └── If Deferred: InjectStartupData() → write env.sh, cmd.sh, .env_ready
            │
            ▼
    run_with_envs.sh unblocks
```

### API Changes

#### New SandboxSet Field: `StartupMode`

```go
// StartupMode defines when the sandbox's main process starts.
// +enum
// +kubebuilder:validation:Enum=Immediate;Deferred
type StartupMode string

const (
    StartupModeDeferred  StartupMode = "Deferred"
    StartupModeImmediate StartupMode = "Immediate"
)
```

Added to `SandboxSetSpec` with `+kubebuilder:default="Immediate"`. The E2B create API is unchanged.

#### New Sandbox Spec Field: `StartupMode`

The SandboxSet controller copies `StartupMode` to the Sandbox CR's spec when it creates
the Sandbox. The sandbox controller reads this field directly during pod generation and initialization.

```go
type SandboxSpec struct {
    // ...existing fields...

    // StartupMode controls when the sandbox's main process starts.
    // Copied from the SandboxSet when the Sandbox is created.
    // +optional
    StartupMode StartupMode `json:"startupMode,omitempty"`
}
```

No separate annotation (e.g., `AnnotationDeferredEnv`) is needed — the CRD field is the
authoritative source.

#### New SandboxClaim Field: `Command`

```go
type SandboxClaimSpec struct {
    // ...existing fields...

    // Command overrides the main container command for the sandboxes claimed by
    // this SandboxClaim. The value is the complete argv and is executed directly
    // without a shell. It takes precedence over the wrapped template command.
    // Only valid when the target SandboxSet uses startupMode: Deferred.
    // This field is immutable once set.
    // +optional
    // +kubebuilder:validation:MinItems=1
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="command is immutable"
    Command []string `json:"command,omitempty"`
}
```

The claimed Sandbox records the command in `AnnotationInitRuntimeRequest` so resume/upgrade replay
executes the same argv. `Immediate` pools reject claims that set this field.

#### Env Vars and Claim Command Persisted via Annotation (Not Container Spec)

Regardless of the startup mode (`Deferred` or `Immediate`), the claim layer **always writes `envVars`
and the optional `command` to the Sandbox's `AnnotationInitRuntimeRequest` annotation**. They are
**not** written to the container spec, because that would trigger the in-place upgrade path which does
not currently support env var changes.

This design ensures:

- Env vars and the claim command survive pod deletion during pause/resume and upgrade.
- The `Initialize` function can always read them from the annotation after pod recreation.
- For `Deferred`: env vars and the claim command are re-written to the filesystem (env.sh, cmd.sh,
  signal file).
- For `Immediate`: they remain in the annotation for `reinitRuntime` but are not pushed to the
  container filesystem.

### Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| `run_with_envs.sh` blocks indefinitely if claim never happens | The pre-warmed sandbox remains in the pool. The SandboxSet controller manages pool lifecycle (scale-down, cleanup). Unclaimed pods are deleted when the SandboxSet scales down or the sandbox TTL expires. |
| Env vars or claim command contain special characters breaking the generated shell fragments | Use single-quote shell escaping (`'` → `'\''`) for both `env.sh` values and `cmd.sh` argv elements. |
| Shared volume adds overhead | `emptyDir` is lightweight; the volume only holds small text files. |
| Backward compatibility with existing sandboxes | Controlled by the `StartupMode` field (default `Immediate`, opt-in to `Deferred`). Existing sandboxes created before this feature are unaffected. |
| Security: env vars visible in `/mnt/envd-claim/env.sh` on disk | File permissions `0600` when the main process runs as root, or `0644` in the per-pod volume when it runs as a non-root user. Secrets should **not** be passed via `envVars`; use the traffic-extension's SecurityProfile for dynamic secret injection via L7 traffic interception. |
| Security: claim `command` allows starting an arbitrary process without `pods/exec` permission | The capability is bounded by the pool image, mounts, and network policy, and is granted by the existing `SandboxClaim` write permission. Documented as a risk; deployments that do not want it must not grant claim creation to untrusted callers. |
| Pod restart loses env.sh, cmd.sh, and signal file (emptyDir ephemeral) | **Handled by design**: The `Initialize` function in `sandbox_initializer.go` re-writes them after every pod recreation (resume/wake, upgrade). The data is persisted in the `AnnotationInitRuntimeRequest` annotation on the Sandbox CR, which survives pod deletion. |
| Command-less `Deferred` pool produces a claim with nothing to run | The claim fails with `InvalidClaimSpec` before any delivery, instead of returning a sandbox whose main process exits immediately. |

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

### Alternative 5: Reject Command-Less `Deferred` Pools

An earlier revision rejected a `Deferred` pool whose main container has no `command`, because replacing
the command makes the image entrypoint unreachable. It is simple and never defers a failure to claim
time, but it forces every author of an entrypoint-based image to duplicate argv in the template. The
current design admits such pools and moves the check to claim validation, where the claim can supply
the argv.

## Upgrade Strategy

- **New sandboxes**: The SandboxSet controller copies `StartupMode` to the Sandbox CR.
  The claim layer writes env vars and the optional command to `AnnotationInitRuntimeRequest`
  (existing `/init` behavior). Command wrapping and filesystem injection are controlled by the
  `StartupMode` field (`Immediate` by default; `Deferred` is opt-in).
- **Existing sandboxes**: No changes needed. Sandboxes without the field behave identically.
- **Agent-runtime image**: Must include `run_with_envs.sh` with both main-command and probe modes;
  the controller's `envd-scripts` init container copies it from the same image (see 1.1).
- **Controller**: Must check `Sandbox.Spec.StartupMode` during pod generation and perform
  command + probe wrapping, and inject the `envd-scripts` init container and the claim volume. Must
  call `InjectStartupData` in the `Initialize` flow for resume/upgrade scenarios when the mode is
  `Deferred`. Must validate claims: `command` on an `Immediate` pool, or no command on either the
  claim or the template of a `Deferred` pool, completes with `InvalidClaimSpec`.
- **Admission**: `Deferred` pools must declare the agent-runtime and mount the envd volume at
  `/mnt/envd` in the main container; `Deferred` with a non-exec probe on an application port is
  rejected. A main-container `command` is optional.
- **SandboxSet CRD**: Requires a new `startupMode` field. Run `make generate manifests`
  after adding the field.
- **Sandbox CRD**: Requires a new `startupMode` spec field (copied from the SandboxSet).
  Run `make generate manifests` after adding the field.
- **SandboxClaim CRD**: Requires a new `command` field and claim validation. Run
  `make generate manifests` after adding the field.
- **E2B API**: unchanged.

### Known Limitations

- **agent-runtime required**: `Deferred` is unavailable to envd-less pools; the wrapper has no
  distribution or delivery path there.
- **Image entrypoint**: an entrypoint-only main container cannot be started with its image default
  once `Deferred` wrapping is applied, because the controller cannot recover the entrypoint argv.
  Such a pool must receive an explicit command from every claim (Story 4); a claim without one fails.
- **Claim command and probes**: a claim command must keep the ports and health endpoints targeted by
  the template probes; the controller cannot validate this.
- **postStartHook**: The container's `postStartHook` is **not** wrapped with `run_with_envs.sh`
  and cannot access per-claim env vars. Users must not rely on per-claim env vars in postStartHook.
- **Non-exec probes**: `httpGet` and `tcpSocket` probes are **not** wrapped with `run_with_envs.sh`
  and do not benefit from probe-mode early-success behavior or per-claim env vars. On `Deferred` pools,
  admission rejects non-exec probes that target application ports; only exec probes and probes
  targeting the runtime's own ports are allowed.
- **`kubectl exec` processes**: This proposal covers the main process and exec probes; new processes
  started by `kubectl exec` are not covered.
- **E2B create API**: no command override; `command` exists only on `SandboxClaim`. Claims created by
  the E2B API never carry a command, so a command-less `Deferred` pool is not usable through E2B.
- **In-place upgrade**: Env vars are not written to the container spec to avoid triggering in-place
  upgrade, which does not support env var changes.

## Implementation History

- [x] 07/01/2026: Proposed design for dynamic environment variable injection
- [x] 10/27/2026: Revised policy placement (pool-level), no-envVars behavior, main-container scope, and file locations
- [x] 10/29/2026: Clarified agent-runtime script distribution, startup ordering, and admission prerequisites (including an explicit main-container command); fixed the claim directory path
- [x] 10/30/2026: Renamed the pool field to `startupMode` (`Immediate`/`Deferred`), made the main-container command optional, and added the `SandboxClaim` command override with its delivery, replay, and validation rules
