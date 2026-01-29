# Contributing to OpenKruise Agents

## Developing Environment

As a contributor, if you want to make any contribution to OpenKruise Agents, we should reach an agreement on the
version of tools used in the development environment. Here are some dependencies with specific versions:

For agent-sandbox-controller:

- Golang : v1.24+
- kubernetes: v1.28+

> You can use `go mod vendor` to avoid the IDE crashes caused by go mod list failures.

## Developing guide

### Generate code and manifests

If the APIs are modified, you have to run the following commands to generate code and manifests.

```bash
# Generate code and manifests e.g. CRD, RBAC YAML files etc
make generate manifests
```

### Run tests

You can use the command `make test` to run unit tests.

For JetBrains IDE users, you can find some pre-configured configurations to run tests directly. They are stored
in [.run](.run)

### Build images

OpenKruise Agents consists of two components:

- agent-sandbox-controller: a k8s operator for agent CRDs, including Sandbox, SandboxSet, etc.
- sandbox-manager: a server for sandbox management, compatible with E2B API.

You can use the commands below to build the images with [Dockerfiles](./dockerfiles).

```shell
make docker-build-controller # build agent-sandbox-controller
make docker-build-manager # build sandbox-manager
make docker-build-runtime # build agent-runtime
```

### Deploy to Kubernetes

It is recommended to deploy OpenKruise Agents to a Kubernetes cluster via kustomize for testing. Any Kubernetes cluster
should be ok, but we recommend using [kind](https://kind.sigs.k8s.io/) to create a local cluster.

1. Modify the following patch files with your own images.
    1. [agent-sandbox-controller](config/default/image_patch.yaml)
    2. [sandbox-manager](config/sandbox-manager/image_patch.yaml)
2. Deploy the components
   ```shell
   make deploy-agent-sandbox-controller
   make deploy-sandbox-manager
   ```

#### Test OpenKruise Agents via E2B client

It is recommended to use the customized e2b protocol with patched client to avoid the complexity of wildcard domain
resolution and certificates.

1. Create a [SandboxSet](examples/code_interpreter/sandboxset.yaml) with name `code-interpreter` as an E2B template
   ```shell
   kubectl apply -f examples/code_interpreter/sandboxset.yaml
   ```

2. Install [E2B](https://e2b.dev) python SDK
   ```shell
   pip install e2b-code-interpreter
   ```

3. Port forward the sandbox-manager
   ```shell
   sudo kubectl port-forward services/sandbox-manager 80:7788 -n sandbox-system
   ```

4. Create a new terminal, and set the environment variables
   ```shell
   export E2B_DOMAIN=localhost
   export E2B_API_KEY=some-api-key # configured in config/sandbox-manager/configuration_patch.yaml
   ```

5. Patch the client (refer to this document)
    1. Copy [patch_e2b.py](sdk/customized_e2b/kruise_agents/patch_e2b.py) to your python project
    2. Patch your E2B SDK before using it
       ```python
       from patch_e2b import patch_e2b
       from e2b_code_interpreter import Sandbox
       
       patch_e2b(False)
       with Sandbox.create(template="code-interpreter") as sbx:
           sbx.run_code("print('hello world')")
       ```

## Proposals

If you are going to contribute a feature with a new API or need significant effort, please submit a proposal
in [./docs/proposals/](./docs/proposals) first.
