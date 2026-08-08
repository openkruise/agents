# Customized E2B SDK patch

This Python library patches the E2B client, converting the native E2B protocol to the OpenKruise Agents private
protocol, thereby simplifying sandbox-manager deployment.

## Problem Statement

The E2B SDK requests the backend using the following protocol:

| Protocol                    | Description          | Example                                |
|-----------------------------|----------------------|----------------------------------------|
| api.E2B_DOMAIN              | Management interface | api.e2b.dev                            |
| \<port\>-\<sid\>.E2B_DOMAIN | Sandbox interface    | 49999-i37sc83s52e2cv85h636jjgs.e2b.dev |

Meanwhile, E2B SDK forces the use of HTTPS.

In our practice, we found that in K8s scenarios, this protocol has the following issues:

1. Requires configuring wildcard domain resolution to the management service (sandbox-manager), unable to use methods
   like hosts for resolution.
2. Requires using expensive wildcard certificates.

The above issues simultaneously make deploying a backend service compatible with E2B have a high threshold: not only
increasing user costs, but also making it difficult to automate the setup of an E2E test environment.

## Usage

Requirements:

- Python 3.9 or newer
- `e2b>=2.8.0`
- `e2b-code-interpreter>=2.4.1`

```python
from kruise_agents.patch_e2b import patch_e2b
from e2b_code_interpreter import Sandbox

patch_e2b(https=False)  # patch sdk

if __name__ == "__main__":
    with Sandbox.create() as sbx:
        sbx.run_code("print('hello world')")
```

## Traffic JWT Refresh

Traffic JWT refresh is an independent, opt-in monkey patch. It requires Python
3.10 or newer, `e2b>=2.35.0,<2.38.0`, and
`e2b-code-interpreter>=2.9.0,<2.10.0`.

```python
from kruise_agents.patch_e2b import patch_e2b
from kruise_agents.patch_traffic_token import patch_traffic_access_token

patch_e2b(https=False)
patch_traffic_access_token()
```

When a Sandbox is configured for Traffic JWT authentication, the patch keeps
its token in memory and refreshes it before expiration. Sync and async envd
HTTP/RPC requests and code-interpreter Jupyter requests read the latest token
immediately before sending. Concurrent refreshes for one Sandbox are combined
into one request. Legacy opaque traffic tokens keep their existing behavior and
do not enable expiration-based refresh.

The async client also runs a proactive refresh task. `kill()`, async context
manager exit, and `await sandbox.close_traffic_token_refresh()` cancel that
task. A terminal `404` or `409` refresh response also stops it. Refresh failures
continue using the previous token while it remains valid; once expired,
data-plane calls fail locally with `TrafficAccessTokenExpired` instead of
sending a known-invalid credential.

An application can explicitly refresh a token when needed:

```python
token = sandbox.refresh_traffic_access_token(force=True)
token = await async_sandbox.refresh_traffic_access_token(force=True)
```

## Rollout

Sandbox-manager now defaults Traffic JWT validity to one hour and caps it at
24 hours. Deploy this patch, or another client with equivalent refresh support,
before enabling the short-lived server policy for existing JWT-authenticated
workloads. Clients that do not refresh will lose data-plane access when their
current token expires.
