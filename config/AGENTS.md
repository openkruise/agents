# Config

## Deployment Configuration Scope

- Deployment manifests and Kustomize deployment configuration in this subtree
  are repository E2E test orchestration, not production configuration. Their
  probes, arguments, and resource settings are not evidence of live deployments
  or production requirements.
- Keep concrete E2E orchestration changes with the corresponding deployment
  work. A design that affects E2E execution should state the need to adjust test
  orchestration without prescribing probe or deployment changes unless the user
  explicitly includes that scope.
- This deployment scope does not change the generated-artifact contract for
  `config/crd/`; follow the repository generation rules for CRD manifests.
