---
title: E2B network and inline SecurityProfile annotations for sandboxes
creation-date: 2026-11-21
last-updated: 2026-11-23
status: provisional
---

# E2B network and inline SecurityProfile annotations for sandboxes

## E2B network annotation

Clients send E2B network settings in the create request body.
By default, sandbox-manager uses a per-Sandbox `TrafficPolicy` for `allowOut` and `denyOut` and the `agents.kruise.io/security-rules` annotation for `network.rules`.
With `--e2b-network-annotation=true`, sandbox-manager stores those settings in the `agents.kruise.io/e2b-network` annotation when claiming a Sandbox from a pool or cloning one from a snapshot.
An explicit native SecurityProfile uses `agents.kruise.io/security-profile` with either flag setting.
Agentio enforces E2B network policy when Sandbox policy discovery is enabled and outbound traffic is routed through its egress gateway.

For example, a client sends this body to `POST /sandboxes`.

```json
{
  "templateID": "my-template",
  "allow_internet_access": false,
  "network": {
    "allowOut": ["api.example.com"],
    "denyOut": ["10.0.0.0/8"],
    "rules": {
      "API.Example.COM": [{"transform": {"headers": {"X-Environment": "test"}}}]
    }
  }
}
```

In annotation mode, the delivered Sandbox has the following metadata.
The annotation value is a JSON string, formatted here for readability.

```yaml
metadata:
  annotations:
    agents.kruise.io/e2b-network: |-
      {
        "allowInternetAccess": false,
        "network": {
          "allowOut": ["api.example.com"],
          "denyOut": ["10.0.0.0/8"],
          "rules": {
            "api.example.com": [{"transform": {"headers": {"x-environment": "test"}}}]
          }
        }
      }
```

| E2B create field | Behavior in annotation mode |
| --- | --- |
| `allow_internet_access` | Stored as `allowInternetAccess`. Omitting it or setting it to `true` keeps the default allow behavior, even with a nonempty `allowOut`. Setting it to `false` adds a final deny for gateway-routed TCP traffic. |
| `network.allowOut` and `network.denyOut` | Both accept IP addresses and CIDRs. `allowOut` also accepts individual domain names. Agentio evaluates E2B allow entries before deny entries. |
| `network.rules` | Maps domains to HTTPS header transforms. Sandbox-manager lowercases domain and header names and combines assignments for the same domain. HTTPS inspection requires the client to trust the gateway CA. |

The E2B API accepts up to 100 entries in each list.
Writing an annotation bypasses the `TrafficPolicy` CRD's limit of 20 destinations per rule.
Domain entries in `allowOut` also enable Agentio's default TCP allowance for `8.8.8.8`.
The E2B API leaves `network.dnsServers` unset. [Agentio gateway policy evaluation](https://github.com/openkruise/agentio/blob/32261ca3ca76d8b84094ff6613e9dd2bfabeaf44/docs/reference/security-profile.md#sandbox-policy-annotations)

When the client omits all network settings, sandbox-manager writes `{"network":{}}` in annotation mode.
Reads and updates select the backend by the presence of the annotation, so switching the flag affects future deliveries.
`GET /sandboxes/{sandboxID}` returns `network.allowOut` and `network.denyOut` when present and omits `network` when both lists are empty.
The Sandbox annotation holds the full policy, including header transforms.

`PUT /sandboxes/{sandboxID}/network` takes the network fields at the top level.
For example, this body replaces `allowOut`, clears the omitted `denyOut`, and preserves the existing header rules.

```json
{"allow_internet_access": false, "allowOut": ["api.example.com", "192.0.2.10"]}
```

Set `allow_internet_access` to `false` on each update that should keep the final deny.
Omitting it restores the default allow behavior.
Omit `rules` to preserve the current transforms or send `"rules": {}` to clear them.

## Inline SecurityProfile for E2B sandboxes

E2B clients can set the `e2b.agents.kruise.io/security-profile` metadata key when creating a sandbox through Kruise Agents.
The value is a JSON string with four optional fields, `rules`, `tcpRules`, `inputs`, and `audit`.
Each uses the corresponding SecurityProfile API type.
This metadata key works with either setting of `--e2b-network-annotation`.
The deployment needs an Agentio gateway with support for inline Sandbox SecurityProfiles to enforce the rules.

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

Sandbox Manager validates the value and writes the `agents.kruise.io/security-profile` annotation on the delivered Sandbox.
`rules` accepts native SecurityProfile actions.
`inputs` supplies named values to the profile.
Top-level `audit` supplies default HTTP audit actions, and a rule can override them with its own `actions.audit`.
TCP rules support `Allow` and `Deny` and keep their declared order.
The maximum JSON size is 32 KiB, with up to 64 HTTP rules and 64 TCP rules.
Use unique names within each rule list and reserve the `agentio-e2b-` prefix for Agentio.

The create endpoint returns HTTP 400 for malformed profile JSON or a request that sets both security metadata keys.
The legacy `e2b.agents.kruise.io/security-rules` key remains available on its existing path.
Use a shared SecurityProfile resource for administrator-managed policy.

Choose one policy format per E2B sandbox.
A SecurityProfile create request returns HTTP 400 when it also supplies `network.allowOut`, `network.denyOut`, or `network.rules` with entries, or sets `allow_internet_access` to `false`.
An empty `network` object and the SDK default `allow_internet_access: true` are accepted.
`PUT /sandboxes/{sandboxID}/network` returns HTTP 400 for a sandbox using a SecurityProfile.
