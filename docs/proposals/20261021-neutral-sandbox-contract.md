# Sandbox lifecycle states

The SandboxSet controller, sandbox-manager and gateway share the state values
in `pkg/sandboxstate`. Go callers using `api/v1alpha1.SandboxStateRunning`
and its siblings migrate to `sandboxstate.Running` and the corresponding
constants. Kubernetes CRD phases and serialized API fields keep their names.

`utils.GetSandboxState` derives the state and a diagnostic reason from one
Sandbox observation. A claimed running instance that is still getting ready
reports `provisioning`. An instance past its shutdown deadline reports
`expired`. Deleted, succeeded, failed and terminating instances report
`dead`. The other states are `creating`, `available`, `running` and
`paused`. A caller that needs the Kubernetes Pending phase reads that phase
directly. Backend reasons are for diagnostics.

SandboxSet groups provisioning instances with used instances. Expired and
dead instances enter the reclamation group. The active count used to refill
claim replicas counts creating, available, running and paused instances.

E2B List, Describe and Delete admit running, paused and provisioning
instances. List and Describe present provisioning instances as running.
List omits other states and Describe returns 404 for them. Owners can
also delete an expired instance. Once deletion is accepted, the manager
removes its route and requests quota release. Expiry alone leaves the
instance counted until deletion or termination is observed. Delete returns
204 for an absent instance or one outside the deletable states.
Manual Pause returns 409 if
the instance is marked for deletion or its shutdown deadline has passed,
even when it is still running and ready. Such a request cannot extend
the instance's retention. Traffic forwarding requires a running
instance with an address. The existing route refresh protocol retains its
deletion markers.
