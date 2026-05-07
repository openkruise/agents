# MEMO

Last updated: 2026-05-07T20:01:34+08:00

## Current Topic

The latest changes implement pause/resume timeout snapshot coordination and policy-based timeout updates across sandbox-manager infra, sandbox-controller, and E2B API flows.

## Compatibility Background (Do Not Delete)

This paragraph is required background information and must not be deleted. The timeout normalization change does not alter the business-level timeout semantics or break compatibility for existing Sandbox objects: old objects keep the same `pauseTime` / `shutdownTime` fields, missing pause-timeout snapshot annotations are handled by the legacy-compatible path, and old versions can ignore the new internal annotation on rollback. The only observable difference is that timeout values written by the new code are normalized to whole-second precision, so a deadline may be advanced by less than one second; this is considered acceptable for the current second-level E2B timeout behavior.

## Latest Progress

- Problem background: `Resume` previously cleared `PauseTime` / `ShutdownTime`, which could leave a sandbox in an accidental never-timeout state if later resume or timeout reset work failed; concurrent paused `ConnectSandbox` calls also needed stable timeout coordination.
- Plan: Centralize timeout persistence behind policy-aware atomic updates, add a pause-cycle timeout snapshot annotation, and make pause/resume/connect/set-timeout flows choose the correct policy.
- Approach: Added `pkg/utils/timeout` helpers for normalized timeout extraction, snapshot set/get/clear/match, equality, and extend-only comparison; timeout values are normalized to whole-second precision before persistence and comparison.
- Code edits: `api/v1alpha1/annotations.go` defines `AnnotationPauseTimeoutSnapshot`; `pkg/sandbox-manager/infra/interface.go` adds timeout update policies/results plus pause/resume options; `pkg/sandbox-manager/infra/sandboxcr/sandbox.go` implements `SaveTimeoutWithPolicy`, captures snapshots on pause, preserves timeouts during resume, and can ensure a missing snapshot before resume.
- Code edits: `pkg/controller/sandbox/sandbox_controller.go` now routes paused reconciliation through `EnsureSandboxPaused` and maintains pause timeout snapshots with conflict retry; `pkg/servers/e2b/pause_resume.go` and `pkg/servers/e2b/timeout.go` use `Always`, `ExtendOnly`, or `SnapshotAware` policies as appropriate; `pkg/sandbox-manager/api.go` forwards `ResumeOptions`.
- Tests: Added or expanded table-driven coverage in `pkg/utils/timeout/timeout_test.go`, `pkg/sandbox-manager/infra/sandboxcr/sandbox_test.go`, `pkg/sandbox-manager/infra/sandboxcr/pause_resume_test.go`, `pkg/controller/sandbox/sandbox_controller_test.go`, and `pkg/servers/e2b/timeout_test.go`; `pkg/servers/e2b/pause_resume_test.go` now checks snapshot creation on pause.
- Local guidance files: Added package-specific `AGENTS.md` files under `pkg/controller/sandbox`, `pkg/sandbox-manager/infra`, and `pkg/sandbox-manager/infra/sandboxcr`.
- Verification: Existing memo recorded `go test ./pkg/sandbox-manager/infra/sandboxcr`, `go test ./pkg/controller/sandbox`, and `go test ./pkg/servers/e2b` as passing; rerun these after any further edits before claiming final readiness.
- Review focus: Confirm the intended lifecycle of the pause snapshot marker, especially retaining it after snapshot-aware reset so concurrent paused `ConnectSandbox` calls get extend-only behavior after the first writer; confirm second-level normalization remains acceptable for all timeout comparisons.
- Risks: Snapshot parsing errors are ignored in some fast paths and treated as missing/overwrite-compatible in others; if annotation corruption should be surfaced differently, adjust policy behavior and tests.
