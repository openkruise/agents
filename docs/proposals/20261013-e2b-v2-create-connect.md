---
title: E2B v2 Create and Connect Routes
authors:
  - "@AiRanthem"
creation-date: 2026-10-13
last-updated: 2026-10-13
status: implemented
---

# E2B v2 Create and Connect Routes

## Summary

e2b SDK 2.51.0 creates a sandbox with `POST /v2/sandboxes` and connects with
`POST /v2/sandboxes/{sandboxID}/connect`. Create reuses the v1 handler. Connect
has its own parser so an omitted body or timeout uses the 300-second default.
`autoPauseMemory` overrides only the memory persistent content for that
sandbox. A malformed create body and a malformed non-empty v2 connect body
return 400.

## Motivation

SDK 2.51.0 no longer creates and connects through the v1 paths. The server
must accept the v2 routes without treating an omitted connect timeout as a
missing required field.

The v2 create body also carries `autoPauseMemory`, the wire field for
`lifecycle.onTimeout.keepMemory`. Callers need a per-sandbox override of the
template memory policy. The rest of the v2 create body stays on the existing
create contract.

### Goals

- Accept `POST /v2/sandboxes` and `POST /v2/sandboxes/{sandboxID}/connect`.
- Apply the 300-second default when a v2 connect omits its body or timeout.
- Let one create request add or remove memory persistence.
- Return 400 for a malformed create body and for a malformed non-empty v2 connect body.

### Non-Goals/Future Work

- A separate v2 create request type or parser.
- Treating an explicit create `timeout` of 0 as invalid. It is indistinguishable
  from omission because the field is an `int`.
- Defaulting an omitted `autoPauseMemory` to true.
- Adding filesystem persistence when `autoPauseMemory` is false.
- Selecting a restore mode from connect `memory`. A paused sandbox keeps the
  persistence policy chosen at creation.
- Changing the v1 connect rule that timeout is required.

## Proposal

### Routes

`POST /v2/sandboxes` calls `CreateSandbox`, the same handler as
`POST /sandboxes`. `POST /v2/sandboxes/{sandboxID}/connect` calls
`ConnectSandboxV2`.

### Create timeout

An omitted `timeout` and an explicit `0` both become 300 seconds before the
range check. Values below the minimum of 30 seconds, other than this rewritten
zero, and values above the configured maximum are rejected with 400.

### autoPauseMemory

The field is a pointer. The e2b 2.51.0 public client omits it unless the caller
sets `lifecycle.onTimeout.keepMemory`.

- Omitted: the sandbox inherits the template memory policy.
- `true`: memory is added to `persistentContents` if it is not already present.
- `false`: memory is removed. Filesystem and other contents are left as they
  are. A template with no filesystem still pauses as pod info only.

The setting is stored on the Sandbox, so a later manual pause uses the same
policy. Recycling restores `persistentContents` from the SandboxSet spec and
does not carry a previous claim's override into the next claim.

### Connect

An empty body, a nil body, or a body without `timeout` uses 300 seconds. An
explicit timeout must be from 1 through the configured maximum; `0` is 400.

`memory` is not decoded. `true` and `false` do not change the timeout and do
not choose a restore mode. The creation-time policy decides whether a paused
sandbox restores memory.

A non-empty body that is not valid JSON, such as `{`, returns 400. An empty
body remains the 300-second default.

### Malformed create JSON

`parseCreateSandboxRequest` returns 400 when the body cannot be decoded. v1 and
v2 create share that parser, so both routes return 400.

## Alternatives

A v2-only create parser could reject `timeout: 0` and default omitted
`autoPauseMemory` to true, matching the generated client's schema comment.
Create stays on the v1 parser so omission and zero keep the existing 300-second
default, and omitted `autoPauseMemory` keeps the template policy instead of
forcing a memory snapshot.

Connect could reject `memory: false` after looking up a paused sandbox. The
field is ignored because resume mode is fixed at creation.

`autoPauseMemory: false` could also insert filesystem persistence. The flag
only adds or removes memory.

## Upgrade Strategy

Existing v1 clients keep their routes. Clusters gain the v2 routes with this
server version. No stored object needs a migration. Sandboxes created before
the field existed keep their template `persistentContents`.

## Additional Details

### Test Plan

- v2 connect with an omitted, empty, or nil body uses 300 seconds.
- v2 connect with `timeout: 0` or a body of `{` returns 400.
- v2 connect with `memory: false` and no timeout uses 300 seconds.
- Create with `{` returns 400.
- Omitted `autoPauseMemory` stays nil. `true` and `false` round-trip, and
  claim or clone persistence adds or drops only memory.

## Implementation History

- 2026-10-13: Record the v2 create and connect contract implemented for e2b SDK 2.51.0.
