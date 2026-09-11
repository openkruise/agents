# Change Log

## v0.6.0-alpha1
> Change log since v0.3.0

Version range: v0.3.0 → v0.6.0-alpha1

---

## 1. Features

### 1.1 Security Enhancement

**Ingress & Egress Control**
- Introduced TrafficPolicy, GlobalTrafficPolicy, and SecurityProfile CRDs to drive sandbox egress control (#397, #433, #445, #448, #483, #494, #521, #588, #610, #615, #745, #746), including protocol fields, scheme matching, CRD registration in kustomization (#915), and restored API definitions (#521).
- SecurityProfile gained MCP tool access-control (#614), `headerManipulation` actions (#829), token-transformation headers (#859), and inline E2B L7 network rules (#838).
- CRD admission validation (#919) and validation/status alignment (#930) were hardened.
- Gateway now supports JWT verification with optional Runtime mTLS (#648, #561), keeps the UUID baseline when JWT is enabled (#885), aligns the traffic token header with the E2B SDK (#689), and rotates traffic access tokens (#742).
- AccessToken is masked in route log output and the debug endpoint (#607).

**Identity & Token Framework**
- Introduced a FeatureGate-controlled Security Identity Provider that issues and propagates tokens across the sandbox lifecycle (#324, #450, #460, #463, #469).
- Token issuance is gated on the `agent-name` label (#488) and deferred until the sandbox reaches Ready (#642); tokens are re-issued after resume and before CSI re-mount (#638).
- Access tokens are now issued at claim time by TokenKind (#671) and on clone (#633), with identity annotations propagated to checkpoints (#637) and storage-auth annotations injected into the clone path (#639).
- Added a SecurityTokenRefreshReconciler for proactive rotation (#475), and refactored `IssueToken` so each provider builds its own request (#632).

**TLS & Runtime Transport**
- Added a gateway CA bundle injection framework (#478) and extended `InjectAllCAIntoContainers` to cover InitContainers (#552).
- TLS-capable sandboxes now route CSI mounts (#720) and the `/init` handshake (#700) over HTTPS; a TLS runtime client wired with Secret-based material is used on claim/clone paths (#702), and the claim path is supplied with the runtime TLS bundle (#729).
- Security tokens are delivered over the resolved runtime transport (#734), and every runtime API call logs the resolved transport (#752). Upgrade hooks use TLS (#886), and self-signed leaf certificates now include SKI/AKI for Python 3.13+ compatibility (#797).


### 1.2 Operations Enhancement

**Checkpoint, Pause/Resume & Commit**
- Introduced `CheckpointControl` for the checkpoint lifecycle (#508) with a `CheckpointRestore` upgrade strategy (#670), `PersistentContents` filesystem checkpoints (#674), selectable checkpoint labels (#712, #714), and a pause path that waits for active checkpoints (#913).
- Added a `Commit` CRD (#502), a Commit controller with registry auth and job orchestration (#533), and a nerdctl commit/push execution layer (#608); commits without a CommitID skip provider deletion and pod deletion is rejected (#595).
- Resume became atomic with a placeholder pause time and a minimum timeout floor (#435), with clearer errors on client cancellation (#424). Post-resume re-runtime-init and CSI re-mount are surfaced via events and conditions (#416).
- A `PauseStrategy` (Stop / Snapshot / CloudDisk) was introduced (#713, #774) and exposed on `SandboxSet` (#839).
- Paused retention timeout handling was refined (#566), and the default failed-sandbox reserve TTL reduced to 30 minutes (#457).

**Upgrade & In-Place Update**
- Paused sandboxes can now be upgraded via `SandboxUpdateOps` (#710) using a two-phase upgrade flow (#750), with upgrade policy cleared on success (#785). The filter was relaxed to accept non-SandboxSet-controlled sandboxes (#482) and the `SandboxHashImmutablePart` check is skipped when the annotation is missing (#531). Only Running/Upgrading sandboxes are eligible candidates (#553), and sandboxes whose template already matches the patch target are skipped (#511).
- Sandbox memory now can be resized during sandbox claims (#519)
- Init-container image consistency is verified before post-resume initialization (#538); resume upgrades continue from the previous failed step (#447); init-container injection order was stabilized for backward compatibility (#513); and injected resources are preserved across resize (#462, #537).
- In-place update false-positives were fixed (#420, #557), and `ResourcesEqual` was renamed to `IsResourceSatisfied` with a relaxed comparison (#716). `SandboxInPlaceResourceResizeGate` was removed from the sandbox-manager layer (#470).

**Observability, Events & Lifecycle Tracing**
- New metrics: `sandbox_runtime_container_abnormal` (#452), `_time` metrics for abnormal states with stale-condition fixes (#591), and metric cleanup moved off the reconcile hot path via an async pool (#461).
- Events and conditions were added for pod creation failures (#626), k8s lifecycle events (#603), and controller/manager lifecycle tracing (#658).
- Proxy and infra reconciler log volume was reduced (#579), and E2B gained an optional dedicated observability listener with the empty debug endpoint removed (#858).

**Controller & SandboxSet**
- `SandboxSet` now auto-creates `SandboxTemplate` (#396), uses a legacy revision hash to prevent sandbox recreation on upgrade (#514), scopes `maxUnavailable` to a startup-failure budget (#910), and sorts scale-down candidates by priority (#803).
- Sandbox finalizer became lazy — added on pause, removed on resume (#646), and leftover pods from a previous same-name sandbox are rejected (#757).
- Status is persisted during the Pending phase (#455), and a batch claim size flag was made effective (#656) with claims scoped to namespace (#824).
- The `okactl` CLI was added for sandbox operations (#497), and multi-arch image publishing was enabled (#545).

**Cache, Informer & Performance**
- Secret-backed key storage switched from a ticker to informer-driven refresh (#421); claim hot path uses `CountActiveSandboxes` (#517); `APIReader` fallbacks were added for claimed-sandbox lookup (#423) and checkpoint wait (#522); `SandboxTemplateRef` is supported in runtime checks (#442); cache misses are returned definitively (#751); and the TrafficPolicy cache is skipped when the CRD is absent (#730).
- Gateway informer cache memory usage was reduced (#724).

**E2B Compatibility**
- Added Claude Code support (#415), pod-IP metadata (#436), E2B ≥v2.25.0 SDK-compatible API key encoding (#473), named cloned sandboxes via metadata extensions (#385), dynamically resolved sandbox domains (#649), and an unlimited default create-server timeout (#484).
- Volume API (#580, #596), Network API (#616), dimension-aware API key quota (#565), a secret-to-MySQL API key migration script (#309), and egress control injection (#397) were added.
- The E2B Volume management endpoints were temporarily disabled (#744).

**Storage & Runtime**
- RRSA-based storage authentication for on-demand CSI mounts (#568), an agent-runtime client with CSI mount API (#685), atomic `ListDir`/`Remove` filesystem operations (#723), and a storage CLI binary (#539).

**Short & Stable Sandbox IDs**
- Implemented short and stable sandbox IDs to reduce identifier length while preserving uniqueness across lifecycle operations (#686).
- Added an atomic `max` helper to support lock-free ID generation utilities (#766).

**Miscellaneous**
- Clone failures are retried (#437, #530, #542); sidecar injection moved into `PodGenerateFunc` (#520); postStart hooks are merged using a `--` separator (#555); the security metadata source was moved to sandbox annotations (#630); and a sync-charts skill was added for CRD/webhook/RBAC/identity synchronization (#916).

### 1.3 Cost Optimization

- **Sandbox recycle / return-to-pool** (#548, #609, #569) — reuse released sandboxes to avoid cold starts.
- **CheckpointRestore upgrade strategy** (#670, #674) — upgrade sandboxes via filesystem checkpoints instead of rebuilds.
- **Auto-pause and resume** (#612, #899) with probe-driven `AutoPausePolicy` (#899) and an `OnIngressTraffic` wake-on-traffic resume rule (#900, #586).
- **PoolAutoscaler** (#625) — capacity-based and cron-driven pool autoscaling with coordinated scale-up execution (#895) and a patched webhook service (#917).
- **Paused retention refinement** (#566) and **DefaultReserveFailedSandboxFor reduced to 30 minutes** (#457).
- **Reconcile hot-path async pool** for metric cleanup (#461) and **CountActiveSandboxes** for the claim hot path (#517).

---

## 2. Bug Fixes

**Core Logic**
- Prevented `ClaimSandbox` from returning `(nil, nil)` on context cancellation (#399); removed `UnsafeDisableDeepCopy` in `groupAllSandboxes` to avoid informer cache corruption (#387).
- Fixed false-positive resource change detection in in-place update (#420, #557), preserved system-injected resource fields during resize (#462, #537), and fixed TTL leak by letting Checkpoint own SandboxTemplate (#419).
- Resume during pausing is rejected with 400 (#404); pausing sandboxes are now allowed to pause (#422); `SandboxSet` legacy revision hash prevents sandbox recreation on upgrade (#514); internal labels no longer leak into sandbox pod templates (#911); invalid `SandboxClaim` retry loops are fixed (#840); sandbox cleanup uses `SandboxManager` on network-policy failures (#707).

**Lifecycle & Status**
- Sandbox status is persisted during the Pending phase (#455); resume flow decouples phase transition from pod readiness (#529); checkpoint delete expectation settles when the checkpoint is already gone (#812); pause conditions for checkpoint-disabled and pod-deleted paths are corrected (#524); pod status is synced before upgrade initialization (#912); pause waits for active checkpoints (#913); `SecurityTokenRefresh` treats absent `RuntimeInitialized` as serving (#675); clone honors request CSI mount config over checkpoint annotation (#641).

**E2B Compatibility**
- Dead sandboxes return 404 from `DescribeSandbox` to avoid SDK `ValueError` (#636, #692); `is_running` is polled after kill to avoid an async-deletion race (#645); pagination is stabilized for duplicate timestamps (#563); reserved failed-sandbox cleanup is fixed (#589); E2B traffic policy precedence is corrected (#740); Volume management endpoints are temporarily disabled (#744); resource ownership and key storage validation are hardened (#835); the configured admin key is persisted and unreadable Secret entries are skipped (#854).

**API Keys & Quota**
- Invalid API key creation returns 400 (#449); the quota anti-drift primary-loss test is stabilized under `-race` (#617); API key persistence and owner labels are hardened (#677); registry secret lookup errors are propagated (#584); the claim batch size flag now takes effect (#656); claims are scoped to namespace (#824).

**Controller / Webhook / CRD**
- Webhook controller queue bootstrap and server CertDir alignment (#654); TrafficPolicy cache setup skipped when the CRD is absent (#730); `SandboxTemplate` webhook registration fixed (#820); `PoolAutoscaler` webhook service patched (#917); `SecurityProfiles`, `GlobalSecurityProfiles`, `GlobalTrafficPolicies` registered in kustomization (#915); ops-template patch sanitization and checkpoint resume selection fixed (#793); `SandboxSet` reconcile is skipped while deleting (#856).

**Gateway & Transport**
- Traffic token header aligned with the E2B SDK (#689); traffic tokens are issued for cloned sandboxes (#728); UUID baseline preserved when JWT auth is enabled (#885); upgrade hooks use TLS (#886); self-signed leaf certificates include SKI/AKI for Python 3.13+ (#797).

**HTTP / Resource Leaks**
- Fixed an unclosed `http.Response` body in the BrowserUse endpoint handler that caused a socket leak (#708).

**Tests / CI Fixes**
- Sandbox connection method in resume (#476); checkpoint condition stabilized with `Eventually` (#592); quota fail-open and resume timeout checks hardened (#884); envoy ext_proc timeout raised and transient 504s tolerated (#906); Redis/CR state dumped on quota rebuild E2E failure (#816); E2B Build Image steps retried on runner resource failures (#647); free-disk-space step added to the e2e-e2b-mysql-latest workflow (#643); flaky background-command kill test stabilized (#651); `TestSandboxManager_DebugMaskAccessToken` stabilized (#634).

---

## 3. Chores

**Dependabot Bumps**
- `aquasecurity/trivy-action` 0.35.0 → 0.36.0 (#294); `github/codeql-action` 4.35.4 → 4.37.8 (#429, #466, #499, #527, #621, #680, #878); `ruby/setup-ruby` 1.307.0 → 1.321.0 (#430, #599, #619, #652, #681); `crate-ci/typos` 1.46.1 → 1.48.0 (#431, #464, #498, #602); `codecov/codecov-action` 6.0.0 → 7.0.0 (#432, #526); `golangci/golangci-lint-action` 9.2.0 → 9.3.0 (#465, #601); `actions/checkout` 6.0.1 → 7.0.1 (#500, #578, #624, #683); `actions/cache` 5.0.5 → 6.1.0 (#575, #600); `docker/setup-qemu-action` 3 → 4 (#622); `spf13/cobra` 1.10.0 → 1.10.2 (#873); `container-storage-interface/spec` 1.9.0 → 1.13.0 (#876); `google.golang.org/protobuf` 1.36.11 → 1.36.12 (#869); `golang-x` group (#868); `otel` group (#867); `zizmorcore/zizmor-action` 0.6.1 → 0.6.2 (#877).

**Documentation & Proposals**
- v0.3.0 changelog (#383); multi-agent development limits in AGENTS.md (#438); pause/resume checkpoint design (#467); sandbox reuse & return-to-pool design (#547); CSI mount proposal (#536); short and stable Sandbox IDs proposal (#635); OpenTelemetry distributed tracing proposal (#604); agent guidance hierarchy refined (#655); proposal authors and image reference (#673).

**CI / Test Infrastructure**
- E2E coverage expanded: fixed E2B 2.24.0 tests (#471), sandbox-manager E2E (#518), E2B create-with-labels and command execution (#582); pytest plugin architecture rewrite and CI updates (#594).
- Envoy base image updated to v1.37.3 (#509).

**Refactors**
- Dependency cleanup breaking circular and layer-violating references (#474); `doSidecarInjection` takes `*Sandbox` (#480); `syncStatusFromPod` extracted as a struct field (#672); sandbox reuse terminology renamed to "recycle" (#609); E2B request context values use an unexported key type (#902); security metadata consumed from sandbox annotations (#630); `IssueToken` no longer takes a request parameter (#632).

**Scripts & Runtime Utilities**
- `run_envd.sh` / `envd-run.sh` updates (#516, #541); `chmod` in runtime function (#486); `RunCommandWithRuntime` timeout (#503).

**Supply-Chain Security**
- CI now runs govulncheck, zizmor, and OpenSSF Scorecard, with gosec enabled (#836), and GitHub Actions hardened against zizmor/Scorecard findings (#921). Tier-1 code-scanning findings (command injection, CVEs, dependabot cooldown) were addressed (#918), gosec warnings were fixed (#587), and a SECURITY.md policy was added (#606).

**Generated Code**
- Generated client update (#417); security-related file relocations (#456).

**Open-Source Storage Tests**
- Added `AgenticBucket` and `BucketSpace` test coverage for open-source storage components (#817).


## New Contributors
* @Kuromesi made their first contribution in https://github.com/openkruise/agents/pull/397
* @oindrilakha12-ui made their first contribution in https://github.com/openkruise/agents/pull/387
* @l1b0k made their first contribution in https://github.com/openkruise/agents/pull/433
* @rakshaak29 made their first contribution in https://github.com/openkruise/agents/pull/442
* @delavet made their first contribution in https://github.com/openkruise/agents/pull/483
* @zyl1121 made their first contribution in https://github.com/openkruise/agents/pull/447
* @Jayant-kernel made their first contribution in https://github.com/openkruise/agents/pull/558
* @denverdino made their first contribution in https://github.com/openkruise/agents/pull/587
* @chacha923 made their first contribution in https://github.com/openkruise/agents/pull/563
* @yanghanlin made their first contribution in https://github.com/openkruise/agents/pull/594
* @Liquorice-Ma made their first contribution in https://github.com/openkruise/agents/pull/497
* @googs1025 made their first contribution in https://github.com/openkruise/agents/pull/545
* @singhsrijan46 made their first contribution in https://github.com/openkruise/agents/pull/613
* @ashnaaseth2325-oss made their first contribution in https://github.com/openkruise/agents/pull/584
* @ZeroCoder-dot made their first contribution in https://github.com/openkruise/agents/pull/673
* @AlbeeSo made their first contribution in https://github.com/openkruise/agents/pull/676
* @vishalmore90 made their first contribution in https://github.com/openkruise/agents/pull/708
* @silver-chard made their first contribution in https://github.com/openkruise/agents/pull/537
* @nishantbkl3345-ship-it made their first contribution in https://github.com/openkruise/agents/pull/798
* @HARSHRAJ2789 made their first contribution in https://github.com/openkruise/agents/pull/790
* @DahuK made their first contribution in https://github.com/openkruise/agents/pull/836
* @chrisliu1995 made their first contribution in https://github.com/openkruise/agents/pull/625
* @omlahore made their first contribution in https://github.com/openkruise/agents/pull/902
* @RedZapdos123 made their first contribution in https://github.com/openkruise/agents/pull/886
* @ywExcellent made their first contribution in https://github.com/openkruise/agents/pull/895

**Full Changelog**: https://github.com/openkruise/agents/compare/v0.3.0...v0.6.0-alpha1

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
