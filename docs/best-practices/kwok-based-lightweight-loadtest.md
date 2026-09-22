# Kwok-based Lightweight Loadtest Framework for OpenKruise Agents

## Summary

This project builds a lightweight, Kwok-based load-testing framework to test OpenKruise Agents' sandbox-provisioning capability under a 100k-scale SandboxSet. It mainly adapts the whole system to sustain a large-scale SandboxSet under Kwok(Kubernetes WithOut Kubelet) through the design of Kwok Stages and tuning of various configurations, and also builds a companion load generator and an automated test workflow.

## Goals

- Measure `sandbox-controller-manager`'s and `sandbox-manager`'s provisioning throughput and latency against a 100k-object `SandboxSet` pool.
- Replace the kubelet/CRI layer with Kwok, so a single machine can host 100k Pods without real compute, and scale — not physical resources — becomes the limiting factor.
- Use `Stage` resources to reproduce realistic Pod-lifecycle with delays and failure rate, instead of a backend that succeeds instantly.
- Generate concurrent load across both E2B and CR paths, and automate the entire cluster-to-teardown lifecycle into a single command.
- Emit a config-driven result for each run, so results stay comparable across different runs, configurations, and code changes.

## How Kwok Works

In node that kwok manages, there is no kubelet and `kwok-controller` substitutes kubelet: it drives Pod's status through the same lifecycle transitions that kubelet would report via its `Stage`, without invoking CRI or allocating real compute.

This framework tests `sandbox-controller-manager`'s and `sandbox-manager`'s performance — such as the manager handing back an already-running Sandbox from the pool, and the controller reconciling — where API objects in etcd are mostly enough, and a running pod is not necessary, since no code needs to run in it. In this way, running resource for 100k sandbox pod can be saved, a single 16-vCPU/128GB-RAM machine is sufficient in this test.

What must avoid triggering is any runtime process, meanly including runtime initialization(use the extension `skip-init-runtime`), CSI volume mounting, security-token issuance and delivery.

## Framework Architecture

In this test, kind is used to quickly build a cluster where test environment runs on it.
There are two nodes in this cluster:
  - The kind cluster provisions one real node, which runs the `Kubernetes control plane`, `kwok-controller`, `sandbox-controller-manager`, and `sandbox-manager`.

  - The second `Node` object is Kwok-managed fake node where all `sandbox Pods` will be placed here by kube-scheduler via taint-toleration mechanism.

`load_generator.go` is the load generator: concurrently issues requests to `sandbox-manager` via E2B, and triggers reconciliation through `SandboxClaim` CRD.

## Kwok Stages

`Stage` is kwok CRD watched by `kwok-controller`. According to stages, `kwok-controller` could fake status transitions a real kubelet would produce.

`kwok-timing-stages.yaml` defines the ones this framework relies on:

- `pod-ready`: matches Pods with `status.phase: Pending`, changes its object state to a Ready pod should have.
- `pod-ready-fail`: same match criteria as `pod-ready`, but changes its object state to a permanently-stuck pod should have.
- `pod-delete`: matches on `deletionTimestamp`, then removes the Pod's object from etcd entirely.
- `pod-inplace-update-complete`: matches Agents' `agents.kruise.io/inplace-update-state` annotation, then changes states to a successfully-updated pod should have.
- `pod-inplace-update-fail`: same match criteria as `pod-inplace-update-complete`, but changes object state to a failed update should have.

Delay exists to stand for a real kubelet's latency, timed from the moment the Pod first matches that `Stage`
's selector: all the stages above resolve to 5–12s, except `pod-delete`, which resolves to 5–10s.

## Load Generator

`load_generator.go` uses goroutines to implement a closed-loop generator, simulating a real multi-operation, concurrent production scenario: `concurrency` worker goroutines each run claim → optional in-place update → optional (pause + resume) → kill synchronously, looping on completion. The CR path runs in a separate goroutine, sequentially issuing `{no-update, with-update}` `SandboxClaim` pairs. Results are received, processed, and output by a goroutine via a shared buffered channel.

## Automation Workflow

`main.go` runs the full test lifecycle end to end: create kind cluster, install kwok-controller and apply the fake
`Node` plus its `Stage` resources, deploy CRDs and components, apply the loadtest `SandboxSet`, run load generator, undeploy everything, delete kind cluster, and prune docker build cache.

`params/main_params.json` configures this orchestration (main.go); `params/generator_params.json` configures the load itself (load_generator.go). Together they hold every parameter meant to be adjusted between test runs.

## Use

### Set Config
Set parameters in `params/generator_params.json` and `params/main_params.json` if needed.

`params/main_params.json`:

| Field | Meaning | Default |
|---|---|---|
| cluster | kind cluster name | "kind" |
| kwok_version | kwok release tag to install | v0.8.0 |
| kind_version | kind version to install | v0.24.0 |
| ns | namespace the agents components deploy into | `sandbox-system` |
| pool_fill_threshold | fraction of `spec.replicas` considered "filled" before moving on (see [Notes](#notes) for why not 1.0) | 0.995 |
| pool_fill_timeout_seconds | hard cap on waiting for the pool to fill | 3600 |

`params/generator_params.json`:

| Field | Meaning | Default |
|---|---|---|
| apiurl | E2B-compatible API base URL the load generator talks to | `http://localhost:7788/kruise/api` |
| apikey | API key sent with every E2B call | `some-api-key` |
| template | `SandboxTemplate` name claims are made against | `loadtest` |
| concurrency | number of closed-loop worker goroutines | 100 |
| duration_seconds | target duration of the E2B load | 60 |
| inplace_update_ratio | fraction of E2B claims that also request an in-place image update | 0.1 |
| pause_resume_ratio | fraction of successful claims followed by a pause + resume | 0.1 |
| inplace_image | image used for in-place-update claims | `busybox:1.36` |
| sandbox_timeout_seconds | absolute delete deadline set at claim time (`claim_time + timeout`) | 180 |
| req_timeout_seconds | HTTP client request timeout applied to every E2B call | 45 |
| out | output directory for the result JSON | `results` |
| cr_claim | whether to also run the `SandboxClaim`-CR sub-path concurrently | true |
| cr_namespace | namespace for CR-path `SandboxClaim`s | `default` |
| cr_replicas | replicas requested per CR-path `SandboxClaim` | 100 |
| cr_count | number of {no-update, with-update} claim pairs run sequentially in the CR sub-path | 2 |
| cr_timeout_seconds | how long to wait for a CR-path claim to reach `Completed` phase | 180 |
| cr_delay_seconds | delay before the CR sub-path starts, letting the E2B load ramp up first | 0 |
| slowest_ids_count | per-op count of slowest request IDs to report| 10 |

### Quick Start

```bash
cd test/kwok_loadtest
go mod tidy
go run -tags auto .
```

Prerequisites: Docker (daemon running), `kubectl`, Go 1.25+.

### Interpreting Results

Each run writes one JSON file to `out` (default `results/`), named
`loadtest-closed-iu<inplace_update_ratio>-pr<pause_resume_ratio>-<timestamp>.json`.

Top-level fields:

- timestamp, wall_s — when the run finished, and the load phase's actual wall-clock duration (excludes cluster setup/teardown).
- total_samples — total `ok + fail` count across every op.
- cr_runs[] — one `{op, latency_ms, ok, error}` object per CR-path execution.
- ops[] — one entry per E2B op (claim, claim_update, pause, resume), each an `opStats` struct: `ok`/`fail` counts, `success_pct`, `throughput_ops_s`, `avg_ms`/`p50`/`p95`/`p99`/`max_ms`, plus `slowest_ids` and `failures[]` below.
- slowest_ids (the `slowest_ids_count` slowest successful samples, descending) and failures[] (`{id, error}` for every failed sample) carry the E2B request ID. Given that ID, logs for that specific request can be grepped directly:

  ```bash
  kubectl -n sandbox-system logs deploy/sandbox-manager | grep 'requestID="<id>"'
  kubectl -n sandbox-system logs deploy/sandbox-controller-manager | grep 'traceID="<id>"'
  ```

  This matches a plain structured log line, not a span/timeline view. A real span timeline is supported via OTLP, but requires enabling `otel` tracing mode and deploying a collector.

## Notes

- General troubleshooting:

  ```bash
  docker ps
  kind get clusters
  kubectl config current-context

  # pool fill progress
  kubectl get sbs loadtest -n default -o jsonpath='{.status.availableReplicas}/{.status.replicas}{"\n"}'
  watch -n5 "kubectl get sbs loadtest -n default -o jsonpath='{.status.availableReplicas}/{.status.replicas}{\"\n\"}'"

  # scheduling problems / component health / logs
  kubectl get pods -n default | grep -v Running | head -20
  kubectl describe pod <pending-pod> -n default
  kubectl -n sandbox-system get pods
  kubectl -n kube-system get pods
  kubectl -n sandbox-system logs deploy/sandbox-controller-manager -f
  kubectl -n sandbox-system logs deploy/sandbox-manager -f
  kubectl -n kube-system logs deploy/kwok-controller -f

  tail -f run.log   # full run output, if main.go was launched detached
  ```

- The current configuration is tuned for 100k-sandbox scale through resource limits and parameters in `manifests/kustomize-overlay`, `manifests/kindnet-resources-patch.yaml` and `params`. Minimum tested hardware: 16 vCPU / 128GB RAM / 100GB storage.

- If you see error or monitor info like OOMKilled, CrashLoopBackOff or 504 Gateway Timeout, check the resource limits and parameters in `manifests/kustomize-overlay`, `manifests/kindnet-resources-patch.yaml` and `params`.

- `SandboxSet` scale-up overshoot: An informer-cache race where the scale-up branch acts on a stale, too-low count with no expectation guard. Self-corrects once the cache catches up — a known product-code gap, not something any config value fixes.

- `sandbox-controller-manager` restarting under the pool-fill reconcile storm: when `kube-apiserver` is slow/overloaded, its leader-election lease renewal call can miss its deadline, controller-runtime treats that as `"leader election lost"`, and the process exits — Kubernetes then restarts the pod. This is a known, accepted risk.

- Synthetic failure injection: `kwok-timing-stages.yaml` sets a ~0.1% failure rate on pod startup and in-place update, simulating a permanent `ImagePullBackOff`. This is why pool fill never reaches 100% and why `claim_update` and `resume` see occasional permanent failures.

- Disk growth across iterations: every run rebuilds both images from scratch, and auto-cleanup only prunes the builder cache, not dangling images. Run `docker image prune -af` periodically during a long iteration session.

