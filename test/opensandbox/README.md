# OpenSandbox data-plane E2E

The suite pins `opensandbox==1.1.0` and uses the same Kind version, component
images and JUnit reporting as E2B E2E. The default six cases correspond to the
four command scenarios and two file round trips in `test/e2b/`. The existing
background kill scenario is adapted to interrupt an active foreground command;
completed background state and logs are outside this delivery.

Both deployments run the same cases from an in-cluster Python Job. Fixtures
exercise SDK create, endpoint discovery for 44772 and 18080, health checks and
actual execution. They verify the SDK's real request destinations and headers.
The optional authentication case follows the existing UUID profile rather than
adding a default security matrix. Signed file URLs remain unsupported.

## Run against a test cluster

Build and load the manager, controller, gateway and runtime images using the
existing Make targets. Deploy the control plane and gateway as in E2B E2E, then:

```sh
bash hack/run-opensandbox-e2e-test.sh both
# Or select one deployment:
bash hack/run-opensandbox-e2e-test.sh ingress
bash hack/run-opensandbox-e2e-test.sh direct
```

The script installs test-only injection templates, enables the OpenSandbox
routes and changes the manager's endpoint strategy between runs. It uses the
current kubectl context and the `default` and `sandbox-system` namespaces. Use
an isolated test cluster. Logs and JUnit XML are written to
`opensandbox-e2e-results/`. The service account can inspect and delete test
Sandboxes and read Pod logs in `default`; it has no Secret access.

For an existing UUID-authenticated gateway profile, set
`OPENSANDBOX_AUTH_E2E=true`. The script does not change gateway authentication.

Failed tests collect status and compatibility-service logs before deleting
instances through Kubernetes. Sandbox IDs are recorded as soon as create
returns, including when subsequent SDK initialization fails. SDK lifecycle
delete, pause/resume and code interpreter are separate deliveries.

## Runtime and endpoint configuration

Enable `--enable-opensandbox-compat` on sandbox-manager and select:

- `--opensandbox-access-mode=ingress` with
  `--opensandbox-gateway-url=https://sandbox.example.com` for an existing TLS
  ingress; HTTP origins are also supported by test deployments.
- `--opensandbox-access-mode=direct` for in-cluster HTTP Pod IP access.

The gateway origin has no path or credentials. SDK 1.1.0 uses its connection
protocol for execution URLs, so its control-plane URL and gateway origin must
use the same scheme. Direct tests use an internal HTTP control-plane URL.

Ingress endpoint responses carry the existing `e2b-sandbox-id`,
`e2b-sandbox-port` and native access credential. They reuse gateway forwarding
and existing UUID/JWT enforcement. TLS terminates at the configured external
ingress; the compatibility listener and gateway-to-sandbox hop use HTTP.
Direct access uses `PodIP:44772`. No new certificates or runtime TLS/mTLS
listener are introduced.

Cold creation selects the existing `agent-runtime` injection plus the generic
`opensandbox-runtime` template in `assets/runtime-injection.yaml`. Pool-backed
workloads and checkpoints need to include this compatibility container. It
calls envd at `127.0.0.1:49983`; commands and files operate inside the workload
container. The envd startup wrapper and native E2B routes remain unchanged.

The normal initialization request optionally records additional runtime ports.
After envd initialization, the same sandbox runtime token is delivered to
44772. The compatibility service verifies it against an already protected
envd, stores it in its own `0600` file and validates
`X-EXECD-ACCESS-TOKEN`. Native initialization records retain this target for
recovery. Initializing and running this HTTP-only compatibility component
against a TLS-only native runtime is unsupported and fails without downgrade.

Both logical execd 44772 and egress 18080 resolve to the real compatibility
listener. Egress policy and Vault requests return HTTP 501 (`NOT_SUPPORTED`).
Foreground shell/argv execution, SSE output, timeout/interruption, and streamed
single/multi-file upload/download are supported. Files use native envd path,
owner and filesystem behavior; explicit upload modes/groups are applied with
literal native command arguments. Byte Range downloads reuse envd handling.
Numeric command UID/GID overrides, line-based download, independent file
management, background state/logs, PTY, code APIs, metrics, signed endpoints and
Lifecycle Server Proxy remain unsupported.
