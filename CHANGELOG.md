# Change Log

## v0.2.0
> Change log since v0.1.0

### Key Features
- Introduced the sandbox-gateway component to separate the data plane (ingress traffic handling) from the component sandbox-manager, enhancing system stability and fault isolation. ([#203](https://github.com/openkruise/agents/pull/203), [@chengzhycn](https://github.com/chengzhycn))
- Added support for mounting multiple NAS/OSS volumes dynamically. ([#211](https://github.com/openkruise/agents/pull/211), [@BH4AWS](https://github.com/BH4AWS))
- Enhanced E2B APIs with snapshot and clone capabilities. ([#204](https://github.com/openkruise/agents/pull/204), [@AiRanthem](https://github.com/AiRanthem))
- Implemented paginated listing and deletion of snapshots. ([#233](https://github.com/openkruise/agents/pull/233), [@AiRanthem](https://github.com/AiRanthem))
- Added protection to prevent unauthorized deletion of Sandbox Pods, and only the sandbox controller may delete them. ([#214](https://github.com/openkruise/agents/pull/214), [@zmberg](https://github.com/zmberg))
- Enabled CSI volume mounting during sandbox creation via SandboxClaim. ([#229](https://github.com/openkruise/agents/pull/229), [@BH4AWS](https://github.com/BH4AWS))
- Added support for automatically injecting runtime and CSI sidecar containers based on sandbox ConfigMap configuration. ([#232](https://github.com/openkruise/agents/pull/232), [@BH4AWS](https://github.com/BH4AWS))

### Performance Improvements
- Improved performance in large-scale sandbox creation scenarios by optimizing ListSandboxesInPool using singleflight deduplication. ([#186](https://github.com/openkruise/agents/pull/186), [@AiRanthem](https://github.com/AiRanthem))
- Introduced feature gate SandboxCreatePodRateLimitGate to enable prioritized sandbox pod creation. ([#171](https://github.com/openkruise/agents/pull/171), [@zmberg](https://github.com/zmberg))

### Other Notable Changes
#### agents-sandbox-manager
- Extended the E2B CreateSandbox API with the e2b.agents.kruise.io/never-timeout annotation to support sandboxes that never auto-delete. ([#183](https://github.com/openkruise/agents/pull/183), [@AiRanthem](https://github.com/AiRanthem))
- Enabled CreateOnNoStock by default when claiming a sandbox. ([#187](https://github.com/openkruise/agents/pull/187), [@AiRanthem](https://github.com/AiRanthem))
- Removed default timeout assignment for paused sandboxes, preventing automatic deletion. ([#196](https://github.com/openkruise/agents/pull/196), [@AiRanthem](https://github.com/AiRanthem))
- Sandbox Manager now supports filtering sandbox-related custom resources via configurable sandbox-namespace and sandbox-label-selector. ([#217](https://github.com/openkruise/agents/pull/217), [@lxs137](https://github.com/lxs137))

#### agents-sandbox-controller
- Add flag parsing support (e.g., -v) for configurable logging verbosity. ([#184](https://github.com/openkruise/agents/pull/184), [@songtao98](https://github.com/songtao98))
- Add label selector for Pod informer to reduce cache size. ([#198](https://github.com/openkruise/agents/pull/198), [@PersistentJZH](https://github.com/PersistentJZH))

### Misc (Chores and tests)
- Docs: add OpenClaw deployment guide. ([#235](https://github.com/openkruise/agents/pull/235), [@bcfre](https://github.com/bcfre))
- docs(AGENTS): add AGENTS.md. ([#237](https://github.com/openkruise/agents/pull/237), [@AiRanthem](https://github.com/AiRanthem))
- Add sandboxSet Prometheus metrics. ([#223](https://github.com/openkruise/agents/pull/223), [@ZhaoQing7892](https://github.com/ZhaoQing7892))
- agent(skills): add detailed deployment skill for Qoder. ([#170](https://github.com/openkruise/agents/pull/170), [@AiRanthem](https://github.com/AiRanthem))

## v0.1.0
### agents-sandbox-controller
- Define and manage sandboxes declaratively using the new Sandbox, SandboxClaim APIs.
- Improve performance with SandboxSet, allowing for faster sandbox creation.

### agents-sandbox-manager
- Supports the E2B mainstream protocol, providing core capabilities such as Agent sandbox creation, routing, and management.
- Extend the E2B protocol to support in-place update image and dynamic mounting of NAS/OSS within the sandbox.
