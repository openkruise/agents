# MEMO

Last updated: 2026-05-08T08:12:58Z

## Current Topic

Added a temporary guard for non-idempotent Resume post operations in `pkg/sandbox-manager/infra/sandboxcr/sandbox.go`.

## Compatibility Background (Do Not Delete)

This paragraph is required background information and must not be deleted. The timeout normalization change does not alter the business-level timeout semantics or break compatibility for existing Sandbox objects: old objects keep the same `pauseTime` / `shutdownTime` fields, missing pause-timeout snapshot annotations are handled by the legacy-compatible path, and old versions can ignore the new internal annotation on rollback. The only observable difference is that timeout values written by the new code are normalized to whole-second precision, so a deadline may be advanced by less than one second; this is considered acceptable for the current second-level E2B timeout behavior.

## Latest Progress

- Problem background: Resume post operations currently run in sandbox-manager and are not idempotent; concurrent Resume calls can duplicate ReInit / CSI re-mount before this logic is moved into agent-runtime.
- Approach: Reuse `retryUpdate`'s boolean return. The first writer is the request that flips `spec.paused` from true to false; non-winners still wait for resume completion and refresh the sandbox, but skip non-idempotent post operations.
- Code edits: `Resume` now stores `resumeUpdated` from `retryUpdate`; after `NewSandboxResumeTask(...).Wait`, post-context handling, `InplaceRefresh`, and `ResourceVersionExpectationExpect`, it returns early for non-winners before ReInit / CSI re-mount.
- Test edits: `pause_resume_test.go` keeps the concurrent Resume success coverage and adds deterministic loser coverage via the already-unpaused latest sandbox path, asserting ReInit is not called when `retryUpdate` skips the update.
- Verification: Passing locally: `go test ./pkg/sandbox-manager/infra/sandboxcr -run 'TestSandbox_ResumeConcurrent|TestSandbox_Resume' -count=1` and `go test ./pkg/sandbox-manager/infra/sandboxcr -count=1`.
- Current changed files: `pkg/sandbox-manager/infra/sandboxcr/sandbox.go`, `pkg/sandbox-manager/infra/sandboxcr/pause_resume_test.go`, `MEMO.md`.
- Review focus: Confirm it is acceptable that non-winners still perform wait + refresh for Resume completion semantics, but skip only ReInit / CSI re-mount.
- Risk: `cachetest.NewTestCache` uses a ResourceVersion interceptor that can allow multiple fake-client concurrent updates to succeed, so the new ReInit call-count assertion is intentionally placed on a deterministic no-update loser scenario rather than the concurrent fake-client test.
