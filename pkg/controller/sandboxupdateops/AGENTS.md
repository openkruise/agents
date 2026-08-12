# SandboxUpdateOps Controller

This package applies a bounded batch update to claimed Sandboxes.

## Local Invariants

- Exclude SandboxSet-controlled pool members; update only eligible claimed
  Sandboxes in the operation's namespace.
- Preserve the raw Strategic Merge Patch so directives such as `$patch` are
  not lost through typed decoding.
- Keep candidate ordering deterministic and account for in-progress and failed
  updates when enforcing `maxUnavailable`.
- Mutation and progress tracking must prevent stale cache observations from
  starting the same update twice.
- Terminal phase calculation, status counters, finalizer cleanup, and removal
  of tracking labels must remain consistent.
