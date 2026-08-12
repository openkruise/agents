# Sandbox ID

This package defines shared, policy-neutral Sandbox identity primitives.

- A non-empty persisted identity is authoritative for the current observation;
  otherwise use the legacy identity. Treat IDs as opaque: never reverse-parse
  or revalidate them during reads.
- Generators require callers to provide a worker ID that is unique across
  processes within the same millisecond. Stay backend-agnostic; callers own
  wraparound, deployment lifecycle, clocks, recovery, contextual length,
  prefix, and replica configuration.
- Enforce only encoding-intrinsic syntax and bit layout.
