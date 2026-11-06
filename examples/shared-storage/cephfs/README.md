# Shared CephFS Volume

Mount one CephFS directory into every sandbox created from a `SandboxSet`, using a statically
provisioned volume served by [ceph-csi](https://github.com/ceph/ceph-csi).

> **Status:** these manifests are schema-checked only and have not been run against a Ceph
> cluster. The `volumeAttributes` and secret keys follow the upstream ceph-csi
> [static PVC guide](https://github.com/ceph/ceph-csi/blob/devel/docs/static-pvc.md). Verify them
> against the ceph-csi version you run.

## Prerequisites

- OpenKruise Agents installed, with a working `sandbox-manager`.
- A Ceph cluster with a CephFS file system, and the ceph-csi CephFS driver
  (`cephfs.csi.ceph.com`) deployed in your cluster, including its `csi-config-map`.
- A directory inside the file system to share, for example `/shared-skills`. It must already
  exist, because static volumes do not create it.
- A Ceph user with access to that file system. Its ID and key go into the Secret.

## 1. Create the secret, volume and claim

Fill in the placeholders in [secret.yaml](secret.yaml) and [pv.yaml](pv.yaml):

| Placeholder | Where to find it |
|-------------|------------------|
| `<ceph-user-id>`, `<ceph-user-key>` | The Ceph user you created for this mount |
| `<ceph-cluster-id>` | The `clusterID` entry in the ceph-csi `csi-config-map` |
| `<cephfs-name>` | `ceph fs ls` |

```bash
kubectl apply -f secret.yaml -f pv.yaml -f pvc.yaml
kubectl get pvc shared-skills   # STATUS should be Bound
```

## 2. Create the SandboxSet

[sandboxset.yaml](sandboxset.yaml) is identical to the [NFS one](../nfs/sandboxset.yaml). The pod
template only references the claim, so the storage backend is chosen by the PV and PVC alone.

```bash
kubectl apply -f sandboxset.yaml
kubectl get sandboxset code-interpreter-shared
```

## 3. Check the mount

```bash
kubectl get pods
kubectl exec <pod-name> -c sandbox -- ls /mnt/shared
```

Every pod of the set should list the same files. If a pod stays in `ContainerCreating`, run
`kubectl describe pod <pod-name>`; mount errors from ceph-csi appear in its events, and the
`csi-cephfsplugin` pod logs on that node have more detail.

To make the volume writable, remove `readOnly: true` from the `volumeMounts` entry, and read the
[notes on isolation](../README.md#things-to-know) first.

## Cleaning up

```bash
kubectl delete -f sandboxset.yaml -f pvc.yaml -f pv.yaml -f secret.yaml
```

The PV uses `persistentVolumeReclaimPolicy: Retain`, so the data in CephFS is kept.
