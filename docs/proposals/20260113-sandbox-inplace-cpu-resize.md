---
title: sandbox inplace cpu resize
authors:
  - "@sivanzcw"
reviewers:
  - "@furykerry"
creation-date: 2026-01-13
last-updated: 2026-01-13
status: implementable
see-also:
replaces:
superseded-by:
---

# Sandbox Inplace CPU Resize

## Table of Contents

- [Title](#title)
    - [Table of Contents](#table-of-contents)
    - [Glossary](#glossary)
    - [Summary](#summary)
    - [Motivation](#motivation)
        - [Goals](#goals)
        - [Non-Goals/Future Work](#non-goalsfuture-work)
    - [Proposal](#proposal)
        - [User Stories](#user-stories)
            - [Story 1](#story-1)
            - [Story 2](#story-2)
        - [Requirements (Optional)](#requirements-optional)
            - [Functional Requirements](#functional-requirements)
                - [FR1](#fr1)
                - [FR2](#fr2)
            - [Non-Functional Requirements](#non-functional-requirements)
                - [NFR1](#nfr1)
                - [NFR2](#nfr2)
        - [Implementation Details/Notes/Constraints](#implementation-detailsnotesconstraints)
        - [Risks and Mitigations](#risks-and-mitigations)
    - [Alternatives](#alternatives)
    - [Upgrade Strategy](#upgrade-strategy)
    - [Additional Details](#additional-details)
        - [Test Plan [optional]](#test-plan-optional)
    - [Implementation History](#implementation-history)

## Summary

This enhancement proposes enabling in-place CPU resizing for sandboxes
allocated from the warm pool through a metadata-based approach.
When a sandbox is claimed via the E2B API, users can specify a CPU scale factor in the metadata
(e.g., `e2b.agents.kruise.io/cpu-scale-factor: 2`).
The sandbox manager will automatically resize the allocated sandbox's CPU resources
in-place using Kubernetes' pod resize subResource, allowing the warm pool to maintain
minimal resource configurations while enabling on-demand CPU scaling for claimed sandboxes.

**Key Benefits**:
- **Cost Optimization**: Maintain warm pools with minimal CPU resources,
scaling up only when sandboxes are actually claimed
- **Zero Downtime**: In-place CPU resizing without pod restart or recreation

## Motivation

### Problem Statement

Currently, the warm pool management strategy requires maintaining sandboxes
with sufficient resources to handle peak workloads. This leads to:

1. **High Resource Costs**: Warm pools must be provisioned with resources sufficient
for the maximum expected workload, even though most sandboxes may not need peak resources immediately
2. **Inefficient Resource Utilization**: Sandboxes sit idle in the warm pool consuming resources
that may never be fully utilized
3. **Limited Flexibility**: Once a sandbox is allocated, its resources cannot be adjusted without recreation,
which causes downtime

### Goals

1. **Enable Metadata-Based CPU Scaling**: Allow users to specify CPU scale factor
via E2B API metadata when creating sandboxes
2. **In-Place Resize**: Leverage Kubernetes pod resize subResource to resize CPU without pod restart
3. **Early Return Support**: Optionally return sandbox immediately
once resize feasibility is confirmed

### Non-Goals/Future Work

1. **Automatic Scaling**: This does not implement automatic CPU scaling based on workload metrics
2. **Resize Policy Configuration**: Users cannot configure resize policies
(always uses `NotRequired` restart policy)

## Proposal

### API Changes

#### E2B API Metadata Extension

The existing E2B `CreateSandbox` API already accepts a `metadata` field.
This enhancement adds support for a new metadata key:

```yaml
metadata:
  e2b.agents.kruise.io/cpu-scale-factor: "2"  # String representation of a positive number
```

**Metadata Key**: `e2b.agents.kruise.io/cpu-scale-factor`
- **Type**: String (must be parseable as a positive float64)
- **Validation**: Must be > 0, typically in range [1, 10] for practical use
- **Default**: If not specified, no resize is performed (backward compatible)

### Design Details

#### Metadata-based CPU Scale Factor

When a sandbox is claimed via `CreateSandbox` API:

1. **Metadata Parsing**: Sandbox manager checks for `e2b.agents.kruise.io/cpu-scale-factor` in the request metadata
2. **CPU Calculation**: If present, calculate target CPU as `originalCPU * scaleFactor`
3. **Validation**: Validate that the target CPU is within acceptable bounds (respects pod limits, resource quotas, etc.)
4. **Resize Trigger**: If validation passes, trigger pod resize via Kubernetes `/resize` subResource

**Example Flow**:
```
Original Sandbox CPU: 1 core
Metadata: e2b.agents.kruise.io/cpu-scale-factor: "2"
Target CPU: 1 * 2 = 2 cores
Action: Resize pod from 1 core to 2 cores
```

#### Sandbox Manager Resize Logic

The resize logic is implemented in the sandbox manager's `ClaimSandbox` flow:

1. **After Sandbox Claim**: Once a sandbox is successfully claimed from the pool
2. **Metadata Check**: Check if `cpu-scale-factor` metadata exists
3. **Current CPU Detection**: Read current CPU from pod spec or status
4. **Target Calculation**: Calculate target CPU = current * scaleFactor
5**Resize Execution**: Call Kubernetes pod `/resize` subresource
6**Status Monitoring**: Monitor pod conditions for resize progress

#### Early Return on Resize Feasibility

**Optional Feature**: Once the system confirms that resize is feasible
(PodResizingInProgress condition is set), the sandbox can be returned to the user immediately,
even if the resize is still in progress.

**Condition Check**:
- Monitor for `PodResizingInProgress` condition in pod status
- Once condition is `True`, resize is confirmed feasible by kubelet
- The condition indicates that:
    - Kubelet has accepted the resize request
    - Resource allocation has been updated
    - Resize is being actuated (may still be in progress)
- Return sandbox to user with status indicating resize in progress
- User can start using sandbox while CPU resize completes asynchronously

#### Flow Diagram

```
User Request (CreateSandbox)
    |
    v
[Parse Metadata]
    |
    v
[cpu-scale-factor present?]
    | No                    Yes
    |  |                     |
    |  v                     v
    |  [Return Sandbox]  [Calculate Target CPU]
    |                          |
    |                          v
    |                     [Validate Feasibility]
    |                          |
    |                    [Infeasible?]
    |                    Yes /  \ No
    |                      |     |
    |                      v     v
    |              [Return Error] [Call Pod /resize]
    |                                 |
    |                                 v
    |                          [Monitor Conditions]
    |                                 |
    |                    [PodResizingInProgress?]
    |                    Yes /          \ No
    |                      |             |
    |                      v             v
    |          [Early Return?]    [Wait for Completion]
    |          Yes /    \ No            |
    |            |       |              |
    |            v       v              v
    |    [Return Sandbox] [Wait]  [Return Sandbox]
    |            |       |              |
    |            +-------+--------------+
    |                     |
    |                     v
    |            [Resize Completes Async]
```

### User Stories

#### Cost-Optimized Warm Pool

As a platform operator, I want to maintain warm pools with minimal CPU resources (0.5 cores) to reduce costs.
When an agent claims a sandbox for a compute-intensive task,
I want the sandbox to automatically scale to 4 cores in-place without downtime.

#### Task-Based Resource Allocation

As an agent developer, I want to specify CPU requirements
when claiming a sandbox based on my task's computational needs,
so that I get appropriate resources without over-provisioning.

#### Immediate Sandbox Availability

As an agent developer, I want to receive the sandbox immediately once
the system confirms that CPU resize is feasible, even if the resize is still in progress,
so that I can start using the sandbox without waiting for resize completion.

### Implementation Details/Notes/Constraints


### Risks and Mitigations

## Alternatives

## Upgrade Strategy

## Additional Details

### Test Plan [optional]

## Implementation History

- [ ] 13/01/2026: Initial proposals draft created
