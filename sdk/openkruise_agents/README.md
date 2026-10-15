# OpenKruise Agents SDK

This package provides the OpenKruise Agents resource-management SDK. It
manages SandboxTemplates, SandboxSets (warm pools), and PoolAutoscalers
through the sandbox-manager resource API at `/agents.kruise.io/v1alpha1`
with `X-API-Key` authentication; end users never hold Kubernetes
credentials. E2B Sandbox compatibility lives in the separate
`sdk/customized_e2b` package.

```python
from openkruise_agents import SandboxSet, Template
from openkruise_agents.models import Container, SandboxSetSpec, SandboxTemplateSpec

build = Template.build(
    name="code-interpreter",
    spec=SandboxTemplateSpec(containers=[Container(
        name="runtime",
        image="example/code-interpreter@sha256:<digest>",
        command=["/usr/local/bin/start"],
    )]),
)
build.wait_until_ready(timeout=300)

pool = SandboxSet.create(
    name="code-interpreter-pool",
    template_ref=build.template_id,
    spec=SandboxSetSpec(replicas=5),
)
pool.wait_until_ready(timeout=300)
```

The client falls back to the `E2B_API_KEY` and `E2B_API_URL` environment
variables, so a deployment that already points the customized E2B SDK at
sandbox-manager needs no extra configuration.

## Kubernetes models

Kubernetes Pydantic models are generated during package build. The generator
downloads the pinned Kubernetes `v1.35.0` swagger document from GitHub,
verifies its SHA-256, extracts the deterministic `$ref` closure reachable from
the SDK's Template roots (preserving every field), and feeds it to
`datamodel-code-generator`. Neither the swagger document nor the generated
model source is tracked in the repository or shipped in the source
distribution; wheels and editable installs generate the model at build time.

Running `pytest` generates the model automatically before test collection; to
generate it manually:

```shell
python scripts/generate_k8s_models.py \
  --output openkruise_agents/models/k8s/__init__.py
```

Generation requires network access to `raw.githubusercontent.com`.
