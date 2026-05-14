# Change Log

## v0.3.0
> Change log since v0.2.0

### Key Features
- Implemented rolling update support for SandboxSet with configurable maxUnavailable policy. ([#256](https://github.com/openkruise/agents/pull/256), [@BITLiutianyang](https://github.com/BITLiutianyang))
- Introduced pluggable KeyStorage with MySQL backend for E2B API key management. ([#291](https://github.com/openkruise/agents/pull/291), [@AiRanthem](https://github.com/AiRanthem))
- Added team-based namespace isolation and team-scoped API key authorization for multi-tenant support. ([#325](https://github.com/openkruise/agents/pull/325), [@AiRanthem](https://github.com/AiRanthem))
- Added Kruise custom path-based routing protocol in sandbox-gateway, supporting `/kruise/{namespace}--{sandbox-name}/{port}/{user-defined-path}` URL format to route requests directly to sandbox pods with path rewrite. ([#278](https://github.com/openkruise/agents/pull/278), [@chengzhycn](https://github.com/chengzhycn))
- Added in-place CPU resize capability when claiming warm pool sandboxes via SandboxClaim or E2B Create API, allowing resource reconfiguration without pod recreation. ([#228](https://github.com/openkruise/agents/pull/228), [@PersistentJZH](https://github.com/PersistentJZH))
- Implemented Recreate upgrade strategy for Sandbox with preUpgrade/postUpgrade lifecycle hooks support. ([#302](https://github.com/openkruise/agents/pull/302), [@zmberg](https://github.com/zmberg))
- Introduced SandboxUpdateOps CR for batch upgrading claimed sandboxes with lifecycle hooks support. ([#307](https://github.com/openkruise/agents/pull/307), [@zmberg](https://github.com/zmberg))
- Added E2B-compatible `GET /templates` and `GET /templates/{templateID}` API endpoints for SandboxTemplate listing and retrieval. ([#265](https://github.com/openkruise/agents/pull/265), [@ZhaoQing7892](https://github.com/ZhaoQing7892))

### Performance Improvements
- Added strategic merge patch markers to CRD types to improve kubectl apply performance and reduce API server load. ([#372](https://github.com/openkruise/agents/pull/372), [@zmberg](https://github.com/zmberg))
- Optimized CSI mounting logic from serial to parallel mounting capability for faster sandbox creation. ([#290](https://github.com/openkruise/agents/pull/290), [@BH4AWS](https://github.com/BH4AWS))
- Added feature gate to cache PodLabelSelector for performance optimization. ([#259](https://github.com/openkruise/agents/pull/259), [@PersistentJZH](https://github.com/PersistentJZH))

### Observability & Metrics
- Added Prometheus metrics for Sandbox, SandboxClaim, SandboxSet and sandbox-manager lifecycle observability. ([#258](https://github.com/openkruise/agents/pull/258), [@liangxiaoping](https://github.com/liangxiaoping); [#292](https://github.com/openkruise/agents/pull/292), [@KeyOfSpectator](https://github.com/KeyOfSpectator))
- Improved claim sandbox failure diagnostics by recording retry pick failures with sandbox key and reason in ClaimMetrics, and exposing aggregated diagnostics in E2B CreateSandbox API errors. ([#356](https://github.com/openkruise/agents/pull/356), [@AiRanthem](https://github.com/AiRanthem))

### Other Notable Changes
#### sandbox-controller
- Added support for negative TTL in SandboxClaim to prevent automatic deletion of the SandboxClaim CR. ([#277](https://github.com/openkruise/agents/pull/277), [@AiRanthem](https://github.com/AiRanthem))
- Introduced SandboxMultiClusterNaming feature gate to embed cluster ID hash in sandbox generateName prefix, preventing name collisions across clusters. ([#370](https://github.com/openkruise/agents/pull/370), [@zmberg](https://github.com/zmberg))
- Added CSI dynamic remounting when resuming sandbox to ensure consistent mount state. ([#305](https://github.com/openkruise/agents/pull/305), [@BH4AWS](https://github.com/BH4AWS))

#### sandbox-manager
- Added custom CDP port support for BrowserUse API, allowing users to specify a cdpPort query parameter to proxy Chrome DevTools Protocol requests. ([#298](https://github.com/openkruise/agents/pull/298), [@AiRanthem](https://github.com/AiRanthem))
- Added support for updating Sandbox and Pod labels during E2B Create Sandbox. ([#201](https://github.com/openkruise/agents/pull/201), [@furykerry](https://github.com/furykerry))

#### Bug Fixes
- Fixed unnecessary InitRuntime execution when no agent-runtime is configured in Sandbox. ([#340](https://github.com/openkruise/agents/pull/340), [@zmberg](https://github.com/zmberg))
- Fixed E2B connect timeout extension semantics to properly handle sandbox lifecycle timeouts. ([#303](https://github.com/openkruise/agents/pull/303), [@AiRanthem](https://github.com/AiRanthem))
- Fixed pause/resume operations to be concurrency-safe under parallel requests. ([#358](https://github.com/openkruise/agents/pull/358), [@AiRanthem](https://github.com/AiRanthem))
- Fixed templateRef sandbox hashing to avoid nil template panic. ([#260](https://github.com/openkruise/agents/pull/260), [@PersistentJZH](https://github.com/PersistentJZH))
- Fixed volume injection issue when user already specified posthook containers. ([#279](https://github.com/openkruise/agents/pull/279), [@BH4AWS](https://github.com/BH4AWS))
- Fixed panic when logging sidecar config errors. ([#301](https://github.com/openkruise/agents/pull/301), [@lxs137](https://github.com/lxs137))
- Updated EnvdVersion from 0.1.1 to 0.2.10 for compatibility. ([#276](https://github.com/openkruise/agents/pull/276), [@AiRanthem](https://github.com/AiRanthem))
- Fixed checkpoint not recording CSI mount state, causing cloned pods to fail mounting. ([#275](https://github.com/openkruise/agents/pull/275), [@BH4AWS](https://github.com/BH4AWS))

#### Security
- Reduced filesystem permissions for certificate and key files to prevent unauthorized access. ([#330](https://github.com/openkruise/agents/pull/330), [@PRAteek-singHWY](https://github.com/PRAteek-singHWY))

### Misc (Chores and tests)
- Added validation for TTLAfterCompleted and WaitReadyTimeout parameters. ([#361](https://github.com/openkruise/agents/pull/361), [@BH4AWS](https://github.com/BH4AWS))
- Implemented validation for SandboxSet volume claim template mounts. ([#359](https://github.com/openkruise/agents/pull/359), [@ajatshatru01](https://github.com/ajatshatru01))
- Added Claude Code deployment guide for AI agent sandbox integration. ([#334](https://github.com/openkruise/agents/pull/334), [@bcfre](https://github.com/bcfre))
- Added comprehensive roadmap for future development. ([#271](https://github.com/openkruise/agents/pull/271), [@furykerry](https://github.com/furykerry))
- Added code-reviewer agents and OWNERS file for maintainership clarity. ([#310](https://github.com/openkruise/agents/pull/310), [@furykerry](https://github.com/furykerry))
- Added fmt-imports.sh script and applied formatting across codebase. ([#272](https://github.com/openkruise/agents/pull/272), [@PersistentJZH](https://github.com/PersistentJZH))

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
