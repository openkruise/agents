# Sharing Storage Across Sandboxes

This directory shows how to give every sandbox created from a `SandboxSet` access to the same
network storage, for example a shared skills directory or a read-only dataset.

The examples use **static volumes**: the volume is declared in the `SandboxSet` pod template as a
regular Kubernetes `PersistentVolumeClaim`, so it works on plain Kubernetes with no OpenKruise
Agents specific storage support.

| Example | Backend | Access mode | Notes |
|---------|---------|-------------|-------|
| [nfs](nfs/README.md) | NFS server | `ReadWriteMany` | Simplest option, a good starting point |
| [cephfs](cephfs/README.md) | CephFS through [ceph-csi](https://github.com/ceph/ceph-csi) | `ReadWriteMany` | Statically provisioned PV |

## Static volumes versus dynamic mounting

OpenKruise Agents also supports [dynamic persistent volume mounting](https://openkruise.io/kruiseagents/user-manuals/sandbox-claim#dynamic-persistent-volume-mounting),
where a volume is attached to a sandbox at claim time (see the
[design proposal](../../docs/proposals/20260608-dynamic-csi-mount.md)). The two approaches differ:

| | Static volume (this directory) | Dynamic mounting |
|---|---|---|
| Declared in | `SandboxSet` pod template | Claim request (`e2b.agents.kruise.io/csi-volume-name` and `csi-mount-point` metadata) |
| Mounted | When the pool pod is created | After the sandbox is claimed |
| Volume per sandbox | Same volume for every sandbox in the set | Can differ per claim |
| Extra components | None | A CSI plugin sidecar in the sandbox, a matching driver registered in agent-runtime |

Use a static volume when every sandbox in a pool should see the same data. Dynamic mounting for
open source backends such as Ceph, JuiceFS and MinIO is tracked in
[#202](https://github.com/openkruise/agents/issues/202) and
[#314](https://github.com/openkruise/agents/issues/314) and is **not** covered here.

## Things to know

- **Every sandbox from the set mounts the same volume.** Writes are visible to all of them. Mount
  the volume with `readOnly: true` unless sandboxes really need to write to shared state.
- **There is no per-tenant isolation.** A sandbox that is claimed by one user can read (and, if
  writable, modify) what every other sandbox from the set sees. Do not put tenant data on a shared
  volume.
- **The PVC must be in the same namespace as the `SandboxSet`.**
- **Use `ReadWriteMany` (or `ReadOnlyMany`) volumes.** A `ReadWriteOnce` volume can be used by
  pods on one node only, which does not fit a pool that spreads across nodes.
- **`volumeClaimTemplates` is a different feature.** It creates one PVC per sandbox, which suits
  private per-sandbox storage, not sharing.

## Verification status

The manifests in this directory were checked by strictly decoding them into the Kubernetes and
OpenKruise Agents API types (unknown or misspelled fields are rejected). They have **not** been
applied to a running cluster. Replace every placeholder (server addresses, cluster IDs, secrets)
and test on your own cluster before relying on them.
