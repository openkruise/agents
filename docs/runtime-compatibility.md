# Container Runtime Compatibility

OpenKruise Agents manages sandboxes as Kubernetes Pods. Because
`spec.template` is a standard `PodTemplateSpec`, users can set
`runtimeClassName` to select an alternative OCI runtime for stronger workload
isolation.

This page documents which OpenKruise Agents features have been validated under
each runtime. The table reflects the current state of testing and known
architectural constraints; entries marked **Untested** mean the feature has not
been explicitly verified but no known blocker exists.

## Compatibility Matrix

| Feature | runc (default) | gVisor (`runsc`) | Kata Containers |
|---|---|---|---|
| Sandbox lifecycle (create, delete, timeout) | ✅ Validated | ✅ Validated | ✅ Expected |
| SandboxSet pre-warming pools | ✅ Validated | ✅ Validated | ✅ Expected |
| SandboxClaim (session binding) | ✅ Validated | ✅ Expected | ✅ Expected |
| Agent-runtime sidecar (envd, file ops, code exec) | ✅ Validated | ✅ Validated | ✅ Expected |
| E2B SDK compatibility | ✅ Validated | ✅ Expected | ✅ Expected |
| In-place image upgrade | ✅ Validated | ❓ Untested | ❓ Untested |
| Pause / Resume | ✅ Validated | ❌ Not supported — gVisor does not support CRIU | ❓ Untested |
| Checkpoint / Fork | ✅ Validated | ❌ Not supported — requires CRIU | ❓ Untested |
| On-Demand CSI Volume Mount | ✅ Validated | ❌ **Incompatible** — see below | ✅ Expected |
| Sandbox Gateway (traffic routing) | ✅ Validated | ✅ Expected | ✅ Expected |

### Legend

- **✅ Validated** — tested and confirmed working.
- **✅ Expected** — no known blocker; uses standard Kubernetes or OCI
  primitives that the runtime supports, but not yet explicitly tested by the
  project.
- **❓ Untested** — not tested; may or may not work. Contributions welcome.
- **❌ Incompatible / Not supported** — known architectural limitation;
  see the notes below.

## Known Incompatibilities

### gVisor + On-Demand CSI Volume Mount

**Status:** Incompatible — architectural limitation of gVisor.

The On-Demand CSI Volume Mount feature
([proposal](proposals/20260608-dynamic-csi-mount.md)) relies on
`mountPropagation: Bidirectional` on a privileged `csi-sidecar` container
(§5.4 of the proposal). This maps to the Linux `rshared` mount option, which
requires the kernel's mount-namespace propagation mechanism.

gVisor is a userspace kernel that does not implement mount-namespace
propagation. It explicitly rejects `rshared` and only permits `private` or
`slave` root mount propagation. When a Pod with `runtimeClassName: gvisor`
contains a container with `mountPropagation: Bidirectional`, the pod sandbox
creation fails with:

```
OCI runtime create failed: root mount propagation option must specify
private or slave: "rshared"
```

This is not a configuration issue — it is a fundamental design constraint of
gVisor's architecture. See [#698](https://github.com/openkruise/agents/issues/698)
for the original bug report and reproduction steps.

**Workaround:** If you need both strong isolation and on-demand CSI mounts,
use Kata Containers (`runtimeClassName: kata`) instead. Kata runs a real Linux
kernel inside a lightweight VM and fully supports bidirectional mount
propagation.

**Using gVisor without CSI mount:** gVisor works well for sandboxes that do
not require on-demand storage attachment. See the
[gVisor example](../examples/gvisor/) for a working SandboxSet configuration.

### gVisor + Pause / Resume (CRIU)

**Status:** Not supported.

Pause/Resume with memory-state preservation depends on
[CRIU](https://criu.org/) (Checkpoint/Restore In Userspace), which operates on
real Linux kernel data structures. gVisor's userspace kernel does not expose
the interfaces that CRIU requires. Pausing a gVisor sandbox will fall back to
container stop/start without memory-state preservation.

## Choosing a Runtime

| Priority | Recommended Runtime |
|---|---|
| Maximum compatibility (all features) | `runc` (default) |
| Strong isolation + on-demand CSI mounts | Kata Containers |
| Strong isolation, no CSI mount needed | gVisor |
| GPU workloads | `runc` (gVisor GPU support is limited) |

## Examples

- [gVisor code-interpreter example](../examples/gvisor/) — SandboxSet with
  `runtimeClassName: gvisor`, demonstrating basic lifecycle and E2B SDK usage
  without CSI sidecar.

## Contributing

If you have validated a feature under a runtime marked **❓ Untested** or
**✅ Expected**, please open a PR updating this table. Testing reports for
Kata Containers and Kuasar are especially welcome.
