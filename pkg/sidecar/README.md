# CustomFuse CSI Sidecar

This directory contains the container images, entrypoint scripts, and example
manifests for the generic FUSE CSI sidecar of OpenKruise Agents
(`customfuseplugin.csi.openkruise.io`). The sidecar runs inside sandbox Pods
and mounts FUSE volumes on demand, without a cluster-level DaemonSet.

Two FUSE clients ship today:

| Directory | Client | Metadata engine | Notes |
|---|---|---|---|
| `juicefs/` | [JuiceFS](https://juicefs.com) | Redis (required) | POSIX semantics, client cache, quota support |
| `s3fs/` | [s3fs](https://github.com/s3fs-fuse/s3fs) | none | Mounts an S3 bucket directly; writes reach object storage immediately |

PVs shared across sandboxes should declare ReadWriteMany. Concurrent
JuiceFS mounts of the same volume are coordinated through the metadata
engine (file locking included), so there is no filesystem-level
corruption; application-level write conflicts remain the caller's
responsibility. s3fs offers no such coordination (see below).
High-risk behaviors are summarized in Pitfalls (at the end of this
document).

## Architecture

The pod-internal view below is driven from outside the pod: a SandboxClaim
(or the E2B extension) references a PV and a Secret — top-level Kubernetes
objects — which the control plane validates before the mount executes
inside the sandbox.

```
SandboxClaim + PV + Secret (top-level objects, validated by the control plane)
        │
        ▼
Sandbox Pod
├── business container (user code)
│     └── storage-cli (installed by the one-shot injected installer, started by envd) — dials the per-driver csi.sock, creates the mountPath symlink
├── csi-sidecar container (privileged)
│     ├── start.sh (PID 1) — supervises the two processes below
│     │     ├── csi-sidecar-customfuse   — CSI node server (csi.sock)
│     │     └── csi-mount-proxy-server   — mount proxy (mounter.sock), runs the FUSE entrypoint per volume
│     └── entrypoint-{juicefs,s3fs}.sh — runs the FUSE mount per volume
└── shared volumes
      ├── customfuse-socket-dir (emptyDir) — socket sharing
      ├── mount-root (emptyDir)            — mount propagation
      └── fuse-device (hostPath /dev/fuse) — kernel FUSE device
```

The mount target lives in the sidecar container under
`/run/csi/mount-root/customfuse/<md5>`; the business container reaches it
through a symlink that storage-cli creates at the user-visible
`mountPath`. `mountPropagation` is `Bidirectional` on the sidecar side and
`HostToContainer` on the business side (see the SandboxSet template
requirements below); without `HostToContainer` on the business side,
mounts made by the sidecar stay invisible in the business container.
`mountPropagation` offers only three values — None, HostToContainer, and
Bidirectional; there is no separate rshared option. The sidecar side
must be Bidirectional and the business side HostToContainer for the
business container to see the mounts, which reach it only through
host-namespace propagation. The mount lands inside the pod's own
`mount-root` emptyDir. The propagated mount is visible in the host mount
namespace (under the pod's kubelet volume path), so host-root processes
can read (and on read-write mounts, modify) it — as they can any pod's
data; read-only mounts stay read-only even for host root, since `ro` is
enforced at the mount level. The mount exists only in this pod's mount
namespace — other pods on the node cannot see it. Node-level trust is
the security boundary.

## Scope

This directory documents the sidecar images and entrypoints. The
SandboxClaim / SandboxSet CRDs and the injection ConfigMap ship with
OpenKruise Agents and must be installed before any of this works. The
control-plane side (CustomFuseMountProvider and the SandboxClaim /
`dynamicVolumesMount` handling) lives in `pkg/agent-runtime/storages` and
`pkg/controller/sandboxclaim`; PVs are static and must exist before the
claim (see the official
[On-Demand Volume Mounting docs](https://openkruise.io/kruiseagents/user-manuals/ondemand-volume-mount)).
The E2B SDK path
reaches the same chain via
the `e2b.agents.kruise.io/csi-volume-config` extension (unit-tested; the
cluster E2E exercised the SandboxClaim path). Cluster E2E covers the
SandboxClaim path with MinIO-backed JuiceFS and s3fs; the OSS-backed
JuiceFS path and stale-mount failure handling **remain unverified** — no
automated E2E coverage, so **validate with your own end-to-end tests
(including fault-injection for the stale-mount paths) before production
use**. Dynamic
mounts are not restored when a sandbox is cloned or resumed from a
Checkpoint; they are re-established only through a fresh claim flow —
after a clone the `mountPath` is just a plain directory in the image (no
error, no mounted data), storage-cli does not self-detect the absence,
only the checks in Verifying a mount reveal it, and recovery means
recreating the sandbox (see Pitfalls, "Clone / Checkpoint restore
silently loses mounts").

## Driver name

`customfuseplugin.csi.openkruise.io` — community-owned, no vendor prefix.

This is not a standard CSI driver for PVC/Pod volume workflows: the sidecar
implements only the CSI Node service (no Controller service), kubelet does
not call it, and the PV serves solely as a configuration carrier for the
SandboxClaim / E2B mount flow. PVC binding and `spec.volumes`-based Pod
usage are not supported — there is no VolumeAttachment and no dynamic
provisioning; every PV must be created statically.

The PV's `spec.csi.driver` must match this name. The mount-proxy component is
built from `kubernetes-sigs/alibaba-cloud-csi-driver` PR #1722 (the customfuse
feature), pinned to a verified commit during the image build; the PR is not
merged upstream, so the pin is load-bearing and upstream rebases require
re-verification. Once upstream merges, point `CSI_DRIVER_REF` at the
merged ref and re-pin the SHA.

This driver does not use the Mount Pod model of the official JuiceFS CSI
driver (`csi.juicefs.com`): the FUSE client runs directly inside the
per-sandbox sidecar container — analogous to the official Sidecar mode,
but scoped to a single sandbox instead of a shared per-node Mount Pod.
Unlike the Mount Pod model, mounts here do not auto-recover when the
client dies (see Shutdown semantics). Each sandbox runs its own client —
there is no cross-sandbox reuse, so plan sidecar resources for large
pools (the same caveat the official Sidecar mode documents).

## Building the images

```bash
# JuiceFS sidecar
docker buildx build -f pkg/sidecar/juicefs/Dockerfile -t <registry>/csi-sidecar-customfuse:juicefs-<ver> .

# s3fs sidecar
docker buildx build -f pkg/sidecar/s3fs/Dockerfile -t <registry>/csi-sidecar-customfuse:s3fs-<ver> .
```

Overseas builders: the default GOPROXY is China-focused — override it
(see the table below) or module downloads may fail.

Stage 1 clones the pinned csi-driver commit from GitHub. For restricted
networks, override the source:

```bash
--build-arg CSI_DRIVER_REPO=<mirror-url> --build-arg CSI_DRIVER_REF=<branch-or-commit-on-the-mirror>
# or pin to a specific commit instead of the PR ref:
--build-arg CSI_DRIVER_REF=<commit-sha> --build-arg CSI_DRIVER_SHA=<same-commit-sha>
```

`pull/1722/head` is a GitHub PR ref, which most mirrors do not carry. PR
refs are inherently mutable (force-pushes and closures move them); the
SHA check is the deliberate guard — a failed build means the source
changed, not a broken build. For production builds, pin a specific
commit SHA rather than any branch or tag, so every build is
reproducible. When overriding `CSI_DRIVER_REPO`/
`CSI_DRIVER_REF`, also set `CSI_DRIVER_SHA` to the commit the mirror
actually serves — the build fails loudly on mismatch.
Restricted networks must also override `IMAGE_PREFIX` so the Docker Hub base
images resolve through an internal registry mirror. The images embed
upstream Apache-2.0 code — keep redistribution compliant (include the
upstream NOTICE and source offer).

Build arguments:

| Argument | Default | Purpose |
|---|---|---|
| `IMAGE_PREFIX` | `docker.io` | Registry prefix for base images — a prefix like `registry.example.com/mirror`, not a full image path |
| `CSI_DRIVER_REPO` | `github.com/kubernetes-sigs/alibaba-cloud-csi-driver.git` | mount-proxy source |
| `CSI_DRIVER_REF` | `pull/1722/head` | ref to fetch from the repo (GitHub PR ref; mirrors must override) |
| `CSI_DRIVER_SHA` | pinned commit (see the Dockerfile) | verified checkout; the build fails when the fetched ref no longer matches this SHA |
| `GOPROXY` | `https://goproxy.cn,direct` | module proxy (China-focused; overseas builders may prefer `https://proxy.golang.org,direct`) |

The sidecar image and the storage-cli installer image are versioned
independently; the wire contract between them is the standard CSI
NodePublishVolume gRPC API (the base64 encoding belongs to the
control-plane transport), so either side can be upgraded alone (a
mismatched pair fails at the CSI request boundary — invalid-argument
errors in the node server logs).

## SandboxSet template requirements

The `customfuse-sidecar` container, the three volumes below, and the
`terminationGracePeriodSeconds` are part of the user's template. The
`runtimes` entries make the controller inject the rest from the
`sandbox-injection-config` ConfigMap (one JSON template per declared
runtime name, in the controller's namespace — `sandbox-system` by
default): the `agent-runtime` template adds the envd installer (a
restartPolicy-Always init container that stays resident) plus a one-shot
storage-cli installer that copies the binary into the shared envd volume —
the business container's envd then starts it (via an injected postStart
hook). If the business container already defines a postStart hook, the
framework merges the injected hook with it — the user's command runs after
envd initialization (`--` separator; a user command containing `--`
passes through unchanged, only the first separator is consumed) — and
env/mount injection still proceeds. A missing ConfigMap or a missing key
for a declared runtime
fails the injection (and thus sandbox creation) loudly; a malformed
JSON template also fails the injection loudly. The `csi`
template can be empty, because the sidecar container itself lives in the
template above. `terminationGracePeriodSeconds` is a
PodSpec field, so it always lives in the template regardless of the
injection arrangement. Container-name conflicts skip that runtime's
injection entirely: this applies to `csi`-template containers colliding
with template containers, and equally in reverse — a template container
named like an injected container (`envd-install`, `storage-cli-install`,
...) silently disables that runtime's injection (see the official
[Runtime Injection docs](https://openkruise.io/kruiseagents/user-manuals/runtime-injection);
detect it by listing the pod's containers — the injected names are simply
missing, with no event). The
framework treats the first container in `spec.containers` as the main
container, so the business container must be listed first — with the
sidecar second, as in the example below.

The framework's generic On-Demand Volume Mounting docs (openkruise.io)
describe fully automatic injection, where the `csi` ConfigMap template
carries the sidecar spec and the framework injects it. Both arrangements
are supported by the same mechanism; the one below — sidecar in the
template, `csi` template empty — is the deployment verified by the cluster
E2E.

```yaml
spec:
  runtimes:
    - name: csi            # enables CSI mount handling (injection template may be empty)
    - name: agent-runtime  # injects the envd (resident) + storage-cli (one-shot) installers
  template:
    spec:
      terminationGracePeriodSeconds: 30   # MUST be >= 30; NOT enforced by the framework — smaller values silently weaken the flush guarantee
      containers:
        - name: sandbox    # business container must run as root (storage-cli dials the 0600 socket)
          image: <business-container-image>
          volumeMounts:
            - { name: customfuse-socket-dir, mountPath: /var/run/csi }
            - { name: mount-root, mountPath: /run/csi/mount-root, mountPropagation: HostToContainer }  # must pair with Bidirectional on the sidecar
        - name: customfuse-sidecar
          image: <registry>/csi-sidecar-customfuse:<juicefs|s3fs>-<ver>  # pick the tag per client
          securityContext: { privileged: true }
          resources:              # MUST set requests/limits — BestEffort pods are OOM-killed first under node pressure
            requests: { cpu: 100m, memory: 256Mi }   # example starting point — size from observed usage
            limits: { cpu: "2", memory: 2Gi }
          volumeMounts:
            - { name: customfuse-socket-dir, mountPath: /var/run/csi }
            - { name: mount-root, mountPath: /run/csi/mount-root, mountPropagation: Bidirectional }  # must pair with HostToContainer on the business container
            - { name: fuse-device, mountPath: /dev/fuse }
      volumes:
        - { name: customfuse-socket-dir, emptyDir: {} }   # keep emptyDir (per-pod scratch)
        - { name: mount-root, emptyDir: {} }              # keep emptyDir
        - { name: fuse-device, hostPath: { path: /dev/fuse, type: CharDevice } }   # type is required — omitting it fails device mounting
```

The two `emptyDir` volumes are per-pod scratch and must start empty — a
persistent volume would leak stale sockets or mount targets across pod
recreations. Plan sidecar resources (each sandbox runs its own client —
see Driver name; size from observed usage, since client memory grows with
cache size); a sidecar hitting its CPU/memory limits degrades mount IO or
gets OOM-killed, breaking the mount. If the sidecar container
crashes, mounts are lost until the sandbox is recreated (see Shutdown
semantics).

The `terminationGracePeriodSeconds >= 30` requirement is load-bearing: the
entrypoint traps SIGTERM and unmounts first so buffered writes are flushed
to object storage. The internal timers nest inside that window (8s gRPC
graceful stop < 10s supervisor escalation < 30s kubelet grace period); a
shorter window lets kubelet's SIGKILL cut the flush short and lose the
last buffered writes. The same 30s applies to the s3fs image: it has no
write cache, but the template is shared by both images and an early
SIGKILL would cut in-flight uploads short (completed objects are already
durable). The framework neither injects nor enforces this value: if it is
left unset, the Kubernetes default of 30s satisfies the requirement — but
set it explicitly, since a cluster-level default can be changed; a smaller
explicit value is not rejected — it silently weakens the guarantee. A
larger value extends only the outer bound — the internal 8s/10s timers
stay fixed.

## JuiceFS entrypoint environment

The entrypoint receives these variables from mount-proxy (populated from the
PV's `volumeAttributes`, the referenced Secret, and the CSI request):

| Variable | Required | Meaning |
|---|---|---|
| `source` | yes | META-URL, e.g. `redis://redis.sandbox-system:6379/0`; embedded userinfo (`redis://user:pass@host`) is masked in logs only — the mount itself receives the real URL. Userinfo embedded here persists in the PV object (etcd); prefer the Secret |
| `mountpoint` | yes | in-sidecar mount target (set by the node server; the entrypoint variable name — distinct from the user-facing claim field `mountPath`) |
| `access_key` / `secret_key` | pair | object storage credentials; trigger the one-time `juicefs format` (idempotent on re-run; a format failure aborts the mount even when a valid token is present) |
| `token` | no | authentication token for an existing file system (JFS_TOKEN). When AK/SK are also present, format still runs (idempotent) and the token authenticates the mount |
| `name` | no | file system name for format (default `myjfs`) |
| `bucket` | for format | S3 bucket; required when AK/SK are present; ignored in token-only mounts |
| `url` | for format | S3 endpoint (`http(s)://host:port`); omitted for AWS S3 |
| `storageType` | no | passed to `--storage` (default `s3`; `minio` maps to `s3`) |
| `format_options` | no | comma-separated `key=value` extra format arguments (e.g. `trash-days=1,block-size=4096`); keys that would override provider-composed flags (`access-key`, `secret-key`, `storage`, `bucket`) are rejected (matched case-sensitively) and fail the mount. Any other key is passed to `juicefs format` as-is — juicefs itself validates it |
| `path` | no | sub-directory mounted as root (`--subdir`); must already exist in the JuiceFS volume (not a local directory) |
| `capacity` | no | quota, e.g. `100Gi` (a plain integer means GiB; units: Ti/TiB, Gi/GiB, Mi/MiB, Ki/KiB); set before mount; the quota is file-system-wide, shared by every sandbox mounting the same volume — one sandbox filling it fails writes for all of them (JuiceFS-only) |
| `readOnly` | no | `"true"` appends `ro`; derived from the claim's readOnly field or the read-only PV access modes (ROX/ReadOnlyMany) — setting it in `volumeAttributes` has no effect |
| `otherOpts` | no | extra mount options (space/comma/tab separated); invalid options fail the mount — check the sidecar logs |

See `juicefs/pv.yaml` and `juicefs/secret.yaml` for a complete example.

## s3fs entrypoint environment

| Variable | Required | Meaning |
|---|---|---|
| `source` | yes | bucket name (`s3://bucket` or bare) |
| `mountpoint` | yes | in-sidecar mount target |
| `access_key` / `secret_key` | yes | credentials written to the s3fs passwd file |
| `url` | no | endpoint for MinIO/OSS/RGW; omit for AWS S3. When set, the entrypoint adds `use_path_request_style` (required by MinIO; do not set a url for AWS S3, which rejects path-style requests) |
| `readOnly` | no | `"true"` appends `ro`; derived from the claim's readOnly field or the read-only PV access modes (ROX/ReadOnlyMany) — setting it in `volumeAttributes` has no effect |
| `otherOpts` | no | extra s3fs options (`debug`/`background` are rejected — see Security design); other invalid options fail s3fs startup — check the sidecar logs |

`path` is not supported (only the bucket root is mounted); passing it
fails the mount. Credentials are read once at mount time — rotation
requires a new sandbox. `storageType` is JuiceFS-only and is ignored
here. s3fs
provides no file locking and only weak cross-node consistency, so
ReadWriteMany is best-effort; concurrent writes to the same object from
multiple sandboxes can silently corrupt it — prefer read-only or
single-writer usage.
See `s3fs/pv.yaml` and `s3fs/secret.yaml` for a complete example.

## Verifying a mount

```bash
# a Sandbox is an OpenKruise Agents custom resource (not a plain Pod)
kubectl get sandbox -n <namespace> -l agents.kruise.io/claim-name=<claim>
kubectl exec <sandbox> -n <namespace> -c sandbox -- sh  # container name per your template
# inside the sandbox's business container
mount | grep -E "juicefs|s3fs"
ls -la <mountPath>   # symlink to /run/csi/mount-root/customfuse/<md5>
```

The `mountPath` must exist in the business image and be empty; a non-empty
target fails the mount. If `mount` prints no juicefs/s3fs line, the mount
never landed (claim failure); a mount entry that returns IO errors means
a stale mount. The detailed error is in `kubectl logs <sandbox> -c
customfuse-sidecar` (the entrypoint logs the cause), while the
SandboxClaim status shows the failure at a high level. Note that failed
sandboxes are deleted and retried by default, which takes the logs with
them; keep the failed sandbox for inspection with the
`e2b.agents.kruise.io/reserve-failed-sandbox` extension (see the official
[Claiming Sandboxes docs](https://openkruise.io/kruiseagents/user-manuals/sandbox-claim)).
A socket-dial
failure at claim time usually means the sidecar was not ready yet: confirm
the `customfuse-sidecar` container is Running and the socket exists —
`ls -la /var/run/csi/sockets/customfuseplugin.csi.openkruise.io/csi.sock`.
(If the business container itself is not Running, check `kubectl logs
<sidecar-pod> -c customfuse-sidecar` first; where kubectl exec is
unavailable, rely on the sidecar logs and the SandboxClaim status.)
Common causes: image still
pulling (transient), missing `privileged` (breaks `/dev/fuse` access —
the runtime's device policy denies the fuse device — and the socket),
or a node without `/dev/fuse` (both permanent).

## Security design

- **Three validation layers** — control-plane provider, sandbox-side node
  server, and entrypoint re-validate the same inputs, because the per-driver
  socket is reachable from inside the sandbox without passing the provider.
- **Credential isolation** — credentials live only in the Secret; they are
  rejected in `volumeAttributes` and masked in logs. The mount request
  itself necessarily carries them (the FUSE client consumes them), which is
  why every log path applies the mask; they never surface in Kubernetes
  events.
- **Privileged sidecar** — the sidecar must be privileged: FUSE
  mount/unmount requires CAP_SYS_ADMIN plus access to the `/dev/fuse`
  device (provided by the `fuse-device` volume). Verified on containerd:
  with `SYS_ADMIN` alone the mount syscall works, but opening `/dev/fuse`
  fails with EPERM — the runtime's devices cgroup denies the fuse device
  to non-privileged containers, and no pod-level setting grants it.
  Reducing the privileges would require runtime-level device
  configuration. Cluster admission policies that forbid privileged
  containers (Pod Security Admission restricted, PodSecurityPolicy, SCC)
  will block the sidecar entirely.
- **Environment hardening** — blocked env keys (BASH_ENV, LD_PRELOAD, PATH,
  ...) are rejected by the provider; the entrypoint unsets the dangerous
  ones and resets PATH to a safe default.
- **Option allowlists** — `debug`/`background` options are rejected by
  the entrypoints, not by the control-plane provider (`debug` would leak
  request details including signed headers into logs; `background` would
  detach the client and defeat the TERM flush).
- **VolumeContext keys** — dangerous env names (BASH_ENV, LD_PRELOAD, ...)
  are rejected as defense in depth for a future mount-proxy revision that
  exports them. Provider-reserved keys (`source`, `url`, ...) are not part
  of this check; they are rejected on Secret keys and mount options
  instead. The current mount-proxy does not pass VolumeContext to the
  entrypoint at all — this layer is inert today.
- **s3fs credential file** — written to a private /tmp path with umask 077;
  it lives for the container's lifetime and disappears with the sandbox
  (it is recreated on each mount attempt, so not finding it after a
  container restart is expected). /tmp is per-container: the business
  container cannot see the file even without the umask.
- **Socket permissions** — the per-driver socket is chmod'd 0600
  (root-only), so the storage-cli in the business container must run as
  root; a non-root business container cannot dial it, and in this version
  has no way to use dynamic mounts at all.

## Shutdown semantics

Sandbox deletion sends SIGTERM to the sidecar container (PID 1 is `start.sh`).
The supervisor forwards the signal to both processes; the entrypoint trap
unmounts first — triggering the FUSE flush of buffered writes — and only then
kills the client. This is what makes the TERM-persistence guarantee hold.
Kubelet signals every container in the pod in parallel, and each gets the
full grace window — the business container exiting early does not shorten
the sidecar's flush window.
When a mount fails, the entrypoint exits non-zero and the error propagates
back through mount-proxy to the NodePublishVolume caller. If the unmount
itself hangs or fails during shutdown, the supervisor's SIGKILL escalation
bounds the window; the flush guarantee holds on the healthy path (a
SIGKILLed client leaves a stale FUSE mount — business-container IO at the
`mountPath` fails, typically with "Transport endpoint is not connected"
(some operations may block briefly until the kernel tears down the FUSE
connection; in rare deadlock cases the block persists until the pod is
destroyed), until the sandbox is recreated; the stale mount lives inside
the pod's
mount namespace and disappears with it — no host cleanup is needed; in
the sidecar log the unmount starts and then stalls until the SIGKILL).
Node OOM-kills and
evictions bypass the TERM flow entirely: buffered writes are lost and the
stale mount persists until the pod is gone (restarting the sidecar
container alone does not clear it).

If the `customfuse-sidecar` container crashes unexpectedly, the mount point
is not automatically restored: kubelet restarts the container, but nothing
re-triggers the mount and there is no supported in-place re-mount —
business-container IO at the `mountPath` errors until the sandbox is
recreated (see Pitfalls, "A sidecar crash loses mounts"). Recover by
deleting the sandbox — the claim
flow then re-provisions a fresh Pod and re-establishes the mount.

## Pitfalls

Quick index of the high-risk behaviors, grouped by category; the
referenced sections carry the details. When troubleshooting, start with
the sidecar container logs, then the SandboxClaim status (see Verifying
a mount). This list covers the high-frequency, high-impact cases only —
it is not an exhaustive fault catalog.

**Configuration**

- Not a standard CSI driver: no PVC binding, no `spec.volumes`-based Pod
  usage (Driver name).
- The business container must run as root; non-root containers cannot use
  dynamic mounts (Security design).
- Do not embed credentials in the `source` userinfo — it persists in the
  PV object (etcd); use the Secret (JuiceFS entrypoint environment).
- Setting `readOnly` in the PV's `volumeAttributes` has no effect —
  mounts stay read-write; use the claim's readOnly field instead (env
  tables).
- For AWS S3, do not set `url` — path-style requests are rejected (s3fs
  entrypoint environment).
- Getting the `mountPropagation` pairing backwards (Bidirectional on the
  business container, HostToContainer on the sidecar) makes mounts
  invisible in the business container (template requirements).
- `terminationGracePeriodSeconds` < 30 is not rejected — it silently
  weakens the flush guarantee (template requirements).
- `customfuse-socket-dir` and `mount-root` must stay emptyDir — a
  PersistentVolume leaks stale sockets and mount targets across pod
  recreations (template requirements).
- `format_options` keys are matched case-sensitively: a case variant of a
  denied key passes the deny-list and fails at juicefs format — the
  sidecar log shows the unknown-flag error from `juicefs format` (JuiceFS
  entrypoint environment).
- `format_options` changes file-system-level configuration that every
  sandbox mounting the volume shares — wrong values affect the whole
  volume, not just one mount (JuiceFS entrypoint environment).

**Runtime failures** (all of these recover only by recreating the
sandbox, which destroys the pod — the business interruption is inherent
to recovery)

- Failed sandboxes are auto-deleted with their logs; use
  `reserve-failed-sandbox` to keep them for inspection (Verifying a
  mount).
- Clone / Checkpoint restore silently loses mounts — verify with the
  checks in Verifying a mount (Scope).
- A sidecar crash loses mounts until the sandbox is recreated; the
  controller performs no in-place repair of a running sandbox's mounts.
  Confirm a stale mount by IO errors at `mountPath` while `mount | grep`
  still shows the entry. Node OOM-kills/evictions skip the flush entirely
  (Shutdown semantics).
- Mount failures during claiming surface in the SandboxClaim status;
  failures after the claim completes (sidecar crash, stale mounts)
  produce no Kubernetes events — only container logs and IO errors.

**Usage risks**

- Host-root processes on the node can read and modify read-write mounts
  — node-level trust is the security boundary (Architecture).
- PV and Secret changes (including credential rotation) do not affect
  running sandboxes — recreate the sandbox for new values.
- Concurrent s3fs writes to the same object can silently corrupt it —
  writes may report success while another sandbox's data is overwritten
  (s3fs entrypoint environment).
- One sandbox filling the shared quota fails writes for every sandbox on
  the volume (JuiceFS-only; s3fs has no quota — see the entrypoint
  environments).
