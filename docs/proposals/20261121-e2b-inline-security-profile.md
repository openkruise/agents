---
title: Inline SecurityProfile for E2B sandboxes
creation-date: 2026-11-21
last-updated: 2026-11-21
status: provisional
---

# Inline SecurityProfile for E2B sandboxes

E2B clients can set the `e2b.agents.kruise.io/security-profile` metadata key
when creating a sandbox through Kruise Agents. The value is a JSON string
containing `rules`, `tcpRules`, or both. The deployment needs an Agentio
gateway with support for inline Sandbox SecurityProfiles to enforce the rules.

```python
import json

from e2b import Sandbox

sandbox = Sandbox.create(
    template="my-template",
    metadata={
        "e2b.agents.kruise.io/security-profile": json.dumps({
            "tcpRules": [
                {
                    "name": "deny-management-ssh",
                    "match": [{"cidrs": ["10.23.0.0/16"], "ports": [22]}],
                    "action": "Deny",
                },
            ],
        }),
    },
)
```

Sandbox Manager validates the value and writes the
`agents.kruise.io/security-profile` annotation on the delivered Sandbox.
HTTP rules support `block` and `headerManipulation`. TCP rules support
`Allow` and `Deny` and keep their declared order. The maximum JSON size is
32 KiB, with up to 64 HTTP rules and 64 TCP rules. Use unique names within
each rule list and reserve the `agentio-e2b-` prefix for Agentio.

A create request returns HTTP 400 if the profile is invalid or the request
includes both security metadata keys.
The legacy `e2b.agents.kruise.io/security-rules` key remains available on its
existing path. Use a shared SecurityProfile resource for administrator-managed
policy.

Choose one policy format per E2B sandbox. A SecurityProfile create request
returns HTTP 400 when it also supplies `network.allowOut`, `network.denyOut`,
or `network.rules` with entries, or sets `allow_internet_access` to `false`.
An empty `network` object and the SDK default `allow_internet_access: true`
are accepted. `PUT /sandboxes/{sandboxID}/network` returns HTTP 400 for a
sandbox using a SecurityProfile.

`--e2b-network-annotation` selects the storage backend for sandboxes using E2B
network settings. An explicit SecurityProfile is written directly to its
Sandbox annotation.
