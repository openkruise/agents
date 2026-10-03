# Shared NFS Volume

Mount one NFS export into every sandbox created from a `SandboxSet`.

## Prerequisites

- OpenKruise Agents installed, with a working `sandbox-manager` (see the
  [installation guide](https://openkruise.io/kruiseagents/installation)).
- An NFS server reachable from every node, exporting a directory you can mount.
- The NFS client tools (for example `nfs-common` or `nfs-utils`) installed on every node that can
  run sandboxes.

## 1. Create the volume and claim

Edit [pv.yaml](pv.yaml) and set `spec.nfs.server` and `spec.nfs.path` to your export, then:

```bash
kubectl apply -f pv.yaml -f pvc.yaml
kubectl get pvc shared-skills   # STATUS should be Bound
```

The PV uses `persistentVolumeReclaimPolicy: Retain`, so deleting the claim never deletes the data
on the NFS server.

## 2. Create the SandboxSet

[sandboxset.yaml](sandboxset.yaml) adds the claim to the pod template and mounts it at
`/mnt/shared`, read-only.

```bash
kubectl apply -f sandboxset.yaml
kubectl get sandboxset code-interpreter-shared
```

## 3. Check the mount

Once the pool sandboxes are running, check from a pod of the set:

```bash
kubectl get pods
kubectl exec <pod-name> -c sandbox -- ls /mnt/shared
```

Every pod of the set should list the same files. To use the sandboxes through the E2B SDK, follow
[examples/code_interpreter](../../code_interpreter/README.md) and use the template
`code-interpreter-shared`.

## Making the volume writable

Remove `readOnly: true` from the `volumeMounts` entry in `sandboxset.yaml`. All sandboxes then
write to the same directory, so avoid concurrent writes to the same file and see the
[notes on isolation](../README.md#things-to-know).

## Cleaning up

```bash
kubectl delete -f sandboxset.yaml -f pvc.yaml -f pv.yaml
```
