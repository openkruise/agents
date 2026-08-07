# Sandbox ID

This package defines shared, policy-neutral Sandbox identity primitives.

- A non-empty persisted identity is authoritative for the current observation;
  otherwise use the legacy identity. Treat IDs as opaque: never reverse-parse
  or revalidate them during reads.
- Generators assume a non-reused, caller-allocated worker incarnation. Stay
  backend-agnostic; callers own cross-process uniqueness, rollout, timing,
  contextual length, prefix, replica configuration, and recovery.
- Enforce only encoding-intrinsic syntax and bit layout.
