# SandboxUpdateOps Controller

This package applies a bounded batch update to claimed Sandboxes.

## Local Invariants

- Exclude SandboxSet-controlled pool members; update only eligible claimed
  Sandboxes in the operation's namespace.
- Preserve the raw Strategic Merge Patch so directives such as `$patch` are
  not lost through typed decoding.
- Keep candidate ordering deterministic and account for in-progress and failed
  updates when enforcing `maxUnavailable`.
- Set the tracking label and resource-version expectation with the Sandbox
  patch so stale cache observations cannot start the same update twice.
- Terminal phase calculation, status counters, finalizer cleanup, and removal
  of tracking labels must remain consistent.
