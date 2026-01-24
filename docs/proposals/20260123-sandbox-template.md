---
title: Add Sandbox Template 
authors:
  - "@furykerry"
reviewers:
  - "@zmberg"
  - "@AiRanthem"
creation-date: 2026-01-23
last-updated: 2026-01-23
status: implementable
---

# Add Sandbox Template

<!-- BEGIN Remove before PR -->
To get started with this template:
1. **Make a copy of this template.**
  Copy this template into `docs/proposals` and name it `YYYYMMDD-my-title.md`, where `YYYYMMDD` is the date the proposal was first drafted.
1. **Fill out the required sections.**
1. **Create a PR.**
  Aim for single topic PRs to keep discussions focused.
  If you disagree with what is already in a document, open a new PR with suggested changes.

The canonical place for the latest set of instructions (and the likely source of this file) is [here](./YYYYMMDD-template.md).

The `Metadata` section above is intended to support the creation of tooling around the proposal process.
This will be a YAML section that is fenced as a code block.
See the proposal process for details on each of these items.

<!-- END Remove before PR -->

## Table of Contents

A table of contents is helpful for quickly jumping to sections of a proposal and for highlighting
any additional information provided beyond the standard proposal template.
[Tools for generating](https://github.com/ekalinin/github-markdown-toc) a table of contents from markdown are available.

- [Title](#title)
  - [Table of Contents](#table-of-contents)
  - [Glossary](#glossary)
  - [Motivation](#motivation)
    - [Goals](#goals)
    - [Non-Goals/Future Work](#non-goalsfuture-work)
  - [Proposal](#proposal)
    - [User Stories](#user-stories)
      - [Story 1](#story-1)
      - [Story 2](#story-2)
    - [Implementation Details/Notes/Constraints](#implementation-detailsnotesconstraints)
    - [Risks and Mitigations](#risks-and-mitigations)
  - [Alternatives](#alternatives)

## Glossary

* SandboxSet: A sandboxset is a collection of sandboxes with the same template that is used for pre-warming sandboxes.
* SandboxTemplate: A sandbox template is a template for creating sandboxes.

## Motivation

- Enable sandbox claim with multiple sandboxsets with different replenishment speed and owning cost
- Enable sandbox claim without any sandboxset. For example, in RL training use cases, the user want to create a sandbox directly from a checkpoint

### Goals

- Add sandbox template CR and enable sandboxset to reference an existing sandbox template
- Add templatePatch field in sandboxset CR to allow multiple subsets using the same template to have differentiated configuration 
- Change the sandboxclaim logic to directly create sandbox if no sandboxset with the same template exists

### Non-Goals/Future Work

- Coordinate multiple sandboxset using the same template during sandboxclaim
- Globally available SandboxTemplate

## API Changes

step 1:  Add a new SandboxTemplate CRD
```
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: code-interpreter-template
  namespace: default
spec:
# checkpointRef:
    name: some-checkpoint
# templateRef:
    name: some-template
    kind: otherTemplateKind 
  template:
    spec:
      containers:
        - name: sandbox
          image: e2bdev/code-interpreter:latest
          resources:
            requests:
              cpu: 1
              memory: 1Gi
            limits:
              cpu: 1
              memory: 1Gi
          env:
            - name: PORT
              value: "49999"
          startupProbe:
            failureThreshold: 20
            httpGet:
              path: /health
              port: 49999
            initialDelaySeconds: 1
            periodSeconds: 2
            timeoutSeconds: 1
          ImagePullPolicy: IfNotPresent
      terminationGracePeriodSeconds: 1

```

step 2-choice A: Enable sandboxset to reference an existing sandbox template
```
apiVersion: agents.kruise.io/v1alpha1
kind: SandboxSet
metadata:
  name: code-interpreter-template
  namespace: default
spec:
  replicas: 10
  templateRef:
    name: code-interpreter-template
  patch:
    spec:
      containers:
      - name: sandbox
        resources:
          limits:
            cpu: "0.5"
            memory: 1Gi
          requests:
          # overwrite the resource usage so that the pre-warmed sandboxset is cheaper to maintain
            cpu: "0.5"
            memory: 1Gi
        env:
        - name: subset
          value: subset-b
```
1. sandboxset controller will use patched podtemplate to create sandboxes, the patch can be arbitrary pod template, e.g. 
   * override the resource usage so that the pre-warmed instanced in scaled up, and during sandbox claim, the sandbox need to be scaled up
   * override pod labels, QoS setting or node affinity, so that the sandbox is provisioned with cheaper but less performant instance.

step 2-choice B: Change the sandbox claim logic to directly create sandbox if no sandboxset with the same template exists, 
add a new metadata

in the sandbox creation request of E2B API: 
```python
sbx = Sandbox.create(template="some-template", timeout=300, metadata={
   "e2b.agents.kruise.io/prewarm-strategy": "none"
})
```

### User Stories

#### Story 1
the infra team of maintain two pool of sandboxes, one small pool with pods of 4c8g and a large pool with pods of 2c8g. 
Most of the time, the user is allocated with sandbox from the small pool, but if the small pool is exhausted, 
the user will be allocated with sandboxes from the large pool, and during sandbox claim, 
the sandbox will be scaled up to 4c8g.

#### Story 2
the user make a checkpoint of sandbox during a RL training, and want to clone two new sandboxes from the sandbox, each with different algorithm strategies.
The user does not want to bother with the sandboxset creation, so he/she just create sandbox with a template and a reference to the checkpoint id. 

### Implementation Details/Notes/Constraints

```
type SandboxTemplate struct {
	// TemplateRef references a SandboxTemplate, which will be used to create the sandbox.
	// +optional
	TemplateRef *SandboxTemplateRef `json:"templateRef,omitempty"`

	// Template describes the pods that will be created.
	// Template is mutual exclusive with TemplateRef
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Template *v1.PodTemplateSpec `json:"template,omitempty"`

	// VolumeClaimTemplates is a list of PVC templates to create for this Sandbox.
	// +optional
	VolumeClaimTemplates []v1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"`
}

type SandboxSetSpec struct {
	// Replicas is the number of unused sandboxes, including available and creating ones.
	Replicas int32 `json:"replicas"`

	// PersistentContents indicates resume pod with persistent content, Enum: ip, memory, filesystem
	PersistentContents []string `json:"persistentContents,omitempty"`

	SandboxTemplate `json:",inline"`
		
	// TemplatePatch is the strategic patch to the pod template
	TemplatePatch runtime.RawExtension `json:"patch,omitempty"`
}
```

webhook changes: 
1. sandboxset webhook should validate: 
   1. the templatePatch is a valid strategic patch, that is, the pod template after the patch should be a valid one. 
   2. the templateRef should point to a sandboxTemplate, and the templateRef of sandboxTemplate should be a pod template.
2. a new webhook for SandboxTemplate should be added to validate the podtemplate is valid.

controller changes: 
1. strategic patch calculation can be costly, sandboxset controller can pre-compute the strategic patch and cache it
2. For sandbox created with a sandboxset which does not have a templateRef, the sandbox will be treated as using a template
with the same name as the sandboxset.
3. when multiple sandboxsets using the same template, the controller will choose the sandboxset without any patch first

### Risks and Mitigations

- Without pre-warming, creating sandbox directly from SandboxTemplate can be lengthy,
sandbox manager may need larger timeout.

## Alternatives

1. the template can be just an abstract name without concrete CR, which is used to group sandboxsets with the same template. 
However, it is hard to ensure the template of these sandboxsets are identical in such case. 
2. the SandboxTemplate can be a cluster-scoped resource. However, in such case, without the separation of namespace, 
the access control of the template will be difficult. We may need a ClusterSandboxTemplate for global available templates.
