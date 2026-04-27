---
name: migrate-clientset-to-client
description: Migrates Kubernetes typed clientset (e.g. client-go kubernetes.Interface or custom clientsets) in test code to the controller-runtime client.Client interface. Use when refactoring test files that call K8sClient.CoreV1().Pods().Get/Create/UpdateStatus typed APIs, replacing them with fake.NewClientBuilder + client.Client.Get/Create/Status().Update patterns. Triggered when the user mentions "replace clientset", "migrate client", or "use controller-runtime client".
---

# Migrate Clientset to controller-runtime client.Client

Migrates typed clientset usage in test code to `client.Client` from `sigs.k8s.io/controller-runtime/pkg/client`.

## Migration Patterns

### Import Changes

**Remove**:
```go
"github.com/openkruise/agents/pkg/sandbox-manager/clients"
// or any other typed clientset packages
```

**Add**:
```go
"k8s.io/apimachinery/pkg/runtime"
utilruntime "k8s.io/apimachinery/pkg/util/runtime"
clientgoscheme "k8s.io/client-go/kubernetes/scheme"
"sigs.k8s.io/controller-runtime/pkg/client"
"sigs.k8s.io/controller-runtime/pkg/client/fake"
```

Also import and register any custom CRD schemes (e.g. `agentsv1alpha1`) if needed.

---

### Building a Fake Client

**Before** (typed clientset):
```go
client := clients.NewFakeClientSet(t)
```

**After** (controller-runtime fake client):
```go
// Initialize scheme at the top of the test function (or outside sub-tests for sharing)
scheme := runtime.NewScheme()
utilruntime.Must(clientgoscheme.AddToScheme(scheme))
utilruntime.Must(agentsv1alpha1.AddToScheme(scheme)) // if CRD support needed

// Build fakeClient in each sub-test
fakeClient := fake.NewClientBuilder().
    WithScheme(scheme).
    WithStatusSubresource(&corev1.Pod{}). // declare types that support status subresource
    Build()
```

> **Important**: `WithStatusSubresource` must declare every type that needs `.Status().Update()`, otherwise the status field will not be persisted.

---

### Creating Resources

**Before**:
```go
createdPod, err := client.K8sClient.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
```

**After**:
```go
err := fakeClient.Create(ctx, pod)
```

---

### Updating Status

**Before**:
```go
createdPod.Status = pod.Status
_, err = client.K8sClient.CoreV1().Pods("default").UpdateStatus(ctx, createdPod, metav1.UpdateOptions{})
```

**After**:
```go
// After Create, the original object already has ResourceVersion populated — update status directly on it
pod.Status = corev1.PodStatus{...}
err = fakeClient.Status().Update(ctx, pod)
```

---

### Reading Resources

**Before**:
```go
pod, err := client.K8sClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
```

**After**:
```go
var pod corev1.Pod
err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pod)
```

> **Important**: Avoid naming the parameter `client` — it conflicts with the `client` package name. Use `c` instead.

---

## Function Signature Migration

**Before**:
```go
func waitForXxx(ctx context.Context, client *SomeClientSet, ...) error {
    pod, err := client.K8sClient.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
```

**After**:
```go
func waitForXxx(ctx context.Context, c client.Client, ...) error {
    var pod corev1.Pod
    if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod); err != nil {
```

---

## Full Migration Example

Real migration in this project: `pkg/sandbox-manager/infra/sandboxcr/claim_test.go`

```go
// Before (TestWaitForPodResizeState)
client := clients.NewFakeClientSet(t)
createdPod, err := client.K8sClient.CoreV1().Pods("default").Create(t.Context(), pod, metav1.CreateOptions{})
createdPod.Status = pod.Status
_, err = client.K8sClient.CoreV1().Pods("default").UpdateStatus(t.Context(), createdPod, metav1.UpdateOptions{})
err = waitForPodResizeState(t.Context(), client, ...)

// After
scheme := runtime.NewScheme()
utilruntime.Must(clientgoscheme.AddToScheme(scheme))
fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).Build()
err := fakeClient.Create(t.Context(), pod)
pod.Status = corev1.PodStatus{...}
err = fakeClient.Status().Update(t.Context(), pod)
err = waitForPodResizeState(t.Context(), fakeClient, ...)
```

---

## Verification

After migration, run:

```bash
go vet ./pkg/...
go test ./pkg/<your-package>/ -count=1 -timeout 120s
```
