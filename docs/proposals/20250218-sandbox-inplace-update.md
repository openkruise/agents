---
title: Sandbox support inplace update image
authors:
  - "@zmberg"
reviewers:
  - "@AiRanthem"
  - "@furykerry"
creation-date: 2025-12-18
status: implementable
---

# Sandbox support inplace update image

## Motivation
In the warm-up pool scenario, a Sandbox may initially use a base image with lower resource allocations. If selected by an upper layer (e.g., E2B), it must be replaced with a specific image and have its resource configuration upgraded.

Therefore, Sandbox must support in-place upgrades for both images and resources.

## Proposal
The steps for in-place upgrades supported by Sandbox are as follows:

**1. Sandbox and Pod contain a revision field.**

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: sample
spec:
  pause: false
  template: 
    ...
status:
  revision: f8d7967bc
  conditions:
    - lastTransitionTime: "2025-12-17T07:34:50Z"
      message: ""
      reason: PodReady
      status: "True"
      type: Ready
    - lastProbeTime: null
      lastTransitionTime: "2025-12-18T04:14:00Z"
      status: "True"
      type: InPlaceUpdateReady
  observedGeneration: 1
  phase: Running
---
apiVersion: v1
kind: Pod
metadata:
  labels:
    pod-template-hash: f8d7967bc
```

revision: Calculated based on the sandbox template, as follows:

```go
by,_ := json.Marshal(box.Spec.Template)
revision := rand.SafeEncodeString(fmt.Sprintf("%x", sha256.Sum256(by)))
```

If the sandbox revision matches the pod's revision, the versions are considered consistent. Otherwise, an in-place upgrade will be triggered.

**2. Update sandbox template image or resources and revision changed.**

When the sandbox template image or resources changed, the revision will change and the condition InPlaceUpdateReady, Ready will be set to false.

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: sample
spec:
  template:
    spec:
      containers:
        - image: from v1 to v2.
status:
  revision: 78bbd5bd4d
  conditions: 
  - lastProbeTime: null
    lastTransitionTime: "2025-12-18T06:14:00Z"
    status: "False"
    type: InPlaceUpdateReady
  - lastTransitionTime: "2025-12-18T06:14:00Z"
    message: ""
    reason: PodReady
    status: "False"
    type: Ready  
  observedGeneration: 2
  phase: Running
```

**3. Sandbox controller detect inconsistency between pod revision and sandbox, triggering an in-place upgrade:**

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    apps.kruise.io/inplace-update-state: '{"revision":"78bbd5bd4d","updateTimestamp":"2025-12-18T04:18:54Z","lastContainerStatuses":{"nginx":{"imageID":"sha256:c073dd278230afed65a306d3614b01913e3c7dc7eefe37f2694b9ed756505118"}},"updateImages":true,"containerBatchesRecord":[{"timestamp":"2025-12-18T04:18:54Z","containers":["nginx"]}]}'
  labels:
    pod-template-hash: 78bbd5bd4d
spec:
  containers:
  - image: from v1 to v2
    
status:
  containerStatuses:
    - containerID: containerd://371f657cdd096059fc614977a6b5f73df4d542fe75faa21b4623e2f658eac091
      image: v1 
      imageID: sha256:c073dd278230afed65a306d3614b01913e3c7dc7eefe37f2694b9ed756505118
      lastState: {}
      name: nginx
      ready: true
      restartCount: 0
      started: true
      state:
        running:
          startedAt: "2025-12-18T04:14:31Z"
```

**4. After the Pod successfully upgraded in place, the sandbox transitioned to the ready state.**

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    apps.kruise.io/inplace-update-state: '{"revision":"78bbd5bd4d","updateTimestamp":"2025-12-18T04:18:54Z","lastContainerStatuses":{"nginx":{"imageID":"sha256:c073dd278230afed65a306d3614b01913e3c7dc7eefe37f2694b9ed756505118"}},"updateImages":true,"containerBatchesRecord":[{"timestamp":"2025-12-18T04:18:54Z","containers":["nginx"]}]}'
  labels:
    pod-template-hash: 78bbd5bd4d
spec:
  containers:
  - image: v2
    
status:
  containerStatuses:
    - containerID: containerd://617571b194040ebb71dff1de4e0fc80363c15ddaebdc83f0167e9664c774d7eb
      image: v2
      imageID: sha256:c073dd278230afed65a306d3614b01913e3c7dc7eefe37f2694b9ed756505118
      lastState:
        terminated:
          containerID: containerd://792c000c3abf7043c46d323a450344a2bfea237619cbf490472c348cc5a86ea7
          exitCode: 0
          finishedAt: "2025-12-18T04:18:55Z"
          reason: Completed
          startedAt: "2025-12-18T04:14:32Z"
      name: nginx
      ready: true
      restartCount: 1
      started: true
      state:
        running:
          startedAt: "2025-12-18T04:18:55Z"
---
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: sample
spec:
  template:
    spec:
      containers:
        - image: v2.
status:
  revision: 78bbd5bd4d
  conditions:
    - lastProbeTime: null
      lastTransitionTime: "2025-12-18T06:14:00Z"
      status: "True"
      type: InPlaceUpdateReady
    - lastTransitionTime: "2025-12-18T06:14:00Z"
      message: ""
      reason: PodReady
      status: "True"
      type: Ready
  observedGeneration: 2
  phase: Running
```

**5. If the user modifies fields other than Image or Resources, an in-place upgrade will not be triggered.**
- The first time sandbox is created, it calculates the hash-without-image-resources.
```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: sample
  annotations:
    sandbox.agents.kruise.io/hash-without-image-resources: "b174ddd7d5ed"
```
- hash-without-image-resources once created, it will not be updated again.
- After the sandbox update, if the hash-without-image-resources changes, it indicates that the sandbox has updated other fields, and an in-place upgrade will not be triggered.
```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: Sandbox
metadata:
  name: sample
spec:
  template:
    ...
status:
  revision: 78bbd5bd4d
  conditions:
    - lastProbeTime: null
      lastTransitionTime: "2025-12-18T06:14:00Z"
      status: "False"
      message: "In-place upgrades only support modifying the image or resources."
      type: InPlaceUpdateReady
    - lastTransitionTime: "2025-12-18T06:14:00Z"
      message: ""
      reason: PodReady
      status: "True"
      type: Ready
  observedGeneration: 2
  phase: Running
```
