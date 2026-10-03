# Running Code Interpreter Under gVisor

This example deploys the E2B code-interpreter sandbox with
[gVisor](https://gvisor.dev/) (`runsc`) as the container runtime, providing
stronger workload isolation than the default `runc` runtime.

For basic concepts (Sandbox, SandboxSet, sandbox-manager, agent-runtime), see
[Running E2B Code Interpreter Sandbox](../code_interpreter/README.md).

## Prerequisites

1. **gVisor installed on every node.** The `runsc` binary and
   `containerd-shim-runsc-v1` must be present and containerd must register a
   runtime handler named **`runsc`** (matching the `RuntimeClass` handler below).
   See the [gVisor containerd quick-start](https://gvisor.dev/docs/user_guide/containerd/quick_start/).
   The relevant section of `/etc/containerd/config.toml`:

   ```toml
   [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
     runtime_type = "io.containerd.runsc.v1"
     [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc.options]
       BinaryName = "/usr/local/bin/runsc"
   ```

2. **A Kubernetes `RuntimeClass` for gVisor:**

   ```shell
   kubectl apply -f - <<EOF
   apiVersion: node.k8s.io/v1
   kind: RuntimeClass
   metadata:
     name: gvisor
   handler: runsc
   EOF
   ```

3. **OpenKruise Agents deployed** (controller + sandbox-manager).

## 1. Deploying the Pre-Warmed Pool

Apply the SandboxSet that uses `runtimeClassName: gvisor`:

```shell
kubectl apply -f examples/gvisor/sandboxset.yaml
```

Verify the sandboxes start under gVisor:

```shell
kubectl get sandbox -n default
kubectl exec <pod-name> -- dmesg | head -5   # should show "Starting gVisor"
```

## 2. Using the Sandbox via E2B SDK

```python
from e2b_code_interpreter import Sandbox

# The template name must match the SandboxSet name
sbx = Sandbox.create(template="code-interpreter-gvisor", timeout=300)
print(f"sandbox id: {sbx.sandbox_id}")

sbx.run_code("import platform; print(platform.platform())")

sbx.kill()
```

## Feature Compatibility Under gVisor

Not all OpenKruise Agents features work under gVisor. See
[Runtime Compatibility](../../docs/runtime-compatibility.md) for the full
matrix. The key limitation:

> **On-Demand CSI Volume Mount is incompatible with gVisor.** gVisor rejects
> `mountPropagation: Bidirectional` (the `rshared` mount option) because it is
> a userspace kernel and does not implement Linux mount-namespace propagation.
> This is an architectural limitation of gVisor, not a configuration issue.
> See [#698](https://github.com/openkruise/agents/issues/698) for details.

This example therefore does **not** include CSI sidecar containers. If you need
both strong isolation and on-demand storage, consider using
[Kata Containers](https://katacontainers.io/) instead (`runtimeClassName: kata`),
which runs a real Linux kernel inside a lightweight VM and fully supports
bidirectional mount propagation.
