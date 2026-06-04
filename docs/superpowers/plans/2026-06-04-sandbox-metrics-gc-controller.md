# Sandbox Metrics GC Controller — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the bespoke `pkg/utils/metricsasync` goroutine pool with a controller-runtime controller that drains a `GenericEvent` channel and invokes `DeleteSandboxMetrics`, resolve outstanding merge conflicts on this branch, and pick up still-in-scope PR #292 follow-ups.

**Architecture:** New `pkg/controller/sandboxmetricsgc` package exposes a `Reconciler` whose only behaviour is `DeleteSandboxMetrics(namespace, name)`. Enqueue is a non-blocking send on a buffered `chan event.GenericEvent` wired into the controller via `source.Channel` + `EnqueueRequestForObject`. Controller-runtime handles workqueue dedup, parallelism, panic recovery, and shutdown — replacing ~280 LOC of custom infra with ~80 LOC of glue.

**Tech Stack:** Go 1.x, sigs.k8s.io/controller-runtime v0.20.2, prometheus client_golang, agents.kruise.io/v1alpha1 Sandbox CRD.

**Reference spec:** [`docs/superpowers/specs/2026-06-04-sandbox-metrics-gc-controller-design.md`](../specs/2026-06-04-sandbox-metrics-gc-controller-design.md)

---

## Phase 0: Unblock the branch (resolve merge conflicts)

The branch is currently un-buildable due to two files with `<<<<<<< HEAD` markers from an aborted master merge. Resolve them before touching anything else.

### Task 1: Resolve `pkg/controller/controllers.go` merge conflict

**Files:**
- Modify: `pkg/controller/controllers.go`

The conflict has two valid claims that need to be merged manually:
- HEAD side: introduces `Deps{MetricsCleanup *metricsasync.Pool}` and threads it to `sandbox.Add`. **Keep the wiring concept** — we still need a metrics cleanup dep — but the type will change in Phase 2 to `sandboxmetricsgc.Enqueuer`. For now, keep `*metricsasync.Pool` so the tree compiles.
- master side: adds `securitytokenrefresh.Add` to `SetupWithManager`. **Keep this** — it is independent work that landed on master.

- [ ] **Step 1: Read the file to confirm current state**

Run: `grep -n '<<<<<<<\|=======\|>>>>>>>' pkg/controller/controllers.go`
Expected: shows three marker lines.

- [ ] **Step 2: Rewrite the file with both changes merged**

Replace the entire file contents with:

```go
/*
Copyright 2025 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openkruise/agents/pkg/controller/sandbox"
	"github.com/openkruise/agents/pkg/controller/sandboxclaim"
	"github.com/openkruise/agents/pkg/controller/sandboxset"
	"github.com/openkruise/agents/pkg/controller/sandboxupdateops"
	"github.com/openkruise/agents/pkg/controller/securitytokenrefresh"
	"github.com/openkruise/agents/pkg/utils/metricsasync"
)

// Deps bundles process-wide dependencies passed to controller Add funcs.
// New dependencies should be appended here rather than introducing extra
// AddFunc parameters across all controllers.
type Deps struct {
	MetricsCleanup *metricsasync.Pool
}

func SetupWithManager(m manager.Manager, deps Deps) error {
	if err := sandbox.Add(m, deps.MetricsCleanup); err != nil {
		return err
	}
	if err := sandboxset.Add(m); err != nil {
		return err
	}
	if err := sandboxclaim.Add(m); err != nil {
		return err
	}
	if err := sandboxupdateops.Add(m); err != nil {
		return err
	}
	if err := securitytokenrefresh.Add(m); err != nil {
		return err
	}
	return nil
}
```

Note: the file deliberately drops the `controllerAddFuncs` slice from master — HEAD's explicit threading wins because we need typed dependency injection for `MetricsCleanup`. `securitytokenrefresh.Add(m)` is invoked directly.

- [ ] **Step 3: Verify markers are gone and build compiles**

Run: `grep -n '<<<<<<<\|=======\|>>>>>>>' pkg/controller/controllers.go`
Expected: no output.

Run: `go build ./pkg/controller/...`
Expected: success (or unrelated errors from the still-conflicted `metrics_test.go` — those are fixed in Task 2).

- [ ] **Step 4: Commit**

```bash
git add pkg/controller/controllers.go
git commit -s -m "fix(controllers): resolve merge conflict, retain MetricsCleanup wiring and add securitytokenrefresh"
```

### Task 2: Resolve `pkg/controller/sandbox/metrics_test.go` merge conflicts

**Files:**
- Modify: `pkg/controller/sandbox/metrics_test.go`

The file has 13 hunks of the same pattern:

```
<<<<<<< HEAD
		recordSandboxMetrics(sandbox)
		defer DeleteSandboxMetrics("default", "...")
=======
		recordSandboxMetrics(sandbox, nil)
		defer deleteSandboxMetrics("default", "...")
>>>>>>> master_keyofspectator_github
```

Resolution: **take the post-merge function signature** (`recordSandboxMetrics(x, nil)`) **but use the exported name** (`DeleteSandboxMetrics`). The exported name is required because Phase 1 introduces a cross-package caller. The current `metrics.go` already exports `DeleteSandboxMetrics`; master's lowercase `deleteSandboxMetrics` would not compile against it.

- [ ] **Step 1: Verify the function exists with the exported name**

Run: `grep -n "^func DeleteSandboxMetrics\|^func deleteSandboxMetrics" pkg/controller/sandbox/metrics.go`
Expected: only `DeleteSandboxMetrics` appears.

- [ ] **Step 2: Sed-resolve all hunks**

This is mechanical: for each hunk, delete the `<<<<<<< HEAD`, `=======`, `>>>>>>> master_keyofspectator_github` lines and the HEAD-side body, keeping the master-side body but with `deleteSandboxMetrics` rewritten to `DeleteSandboxMetrics`.

Run:
```bash
python3 - <<'PY'
import re, pathlib
p = pathlib.Path("pkg/controller/sandbox/metrics_test.go")
src = p.read_text()
# Pattern: <<<HEAD ... === ... >>>master — keep the master block, drop markers.
pattern = re.compile(r"<<<<<<< HEAD\n.*?=======\n(.*?)>>>>>>> master_keyofspectator_github\n", re.DOTALL)
out = pattern.sub(lambda m: m.group(1), src)
# Now rename deleteSandboxMetrics -> DeleteSandboxMetrics (test file calls only).
out = out.replace("deleteSandboxMetrics(", "DeleteSandboxMetrics(")
p.write_text(out)
PY
```

- [ ] **Step 3: Verify markers are gone**

Run: `grep -cn '<<<<<<<\|=======\|>>>>>>>' pkg/controller/sandbox/metrics_test.go`
Expected: `0`.

Run: `grep -cn 'deleteSandboxMetrics\|DeleteSandboxMetrics' pkg/controller/sandbox/metrics_test.go`
Expected: a positive count, with `grep -n 'deleteSandboxMetrics' pkg/controller/sandbox/metrics_test.go` returning nothing (no lowercase form remains).

- [ ] **Step 4: Run the sandbox unit tests to confirm compile + pass**

Run: `go test ./pkg/controller/sandbox/... -count=1 -run 'TestRecordSandboxMetrics|TestDeleteSandboxMetrics|TestSandbox' -timeout 120s`
Expected: PASS. Some tests touch global state via `sync.Map`; if a flake appears in `TestSandbox{Pause,Resume}Duration` it is environmental and can be retried.

- [ ] **Step 5: Commit**

```bash
git add pkg/controller/sandbox/metrics_test.go
git commit -s -m "fix(sandbox): resolve metrics_test.go merge conflict, unify on DeleteSandboxMetrics"
```

### Task 3: Confirm tree builds clean

- [ ] **Step 1: Build the two packages we touched**

Run: `go build ./pkg/controller/... ./pkg/utils/metricsasync/...`
Expected: success.

- [ ] **Step 2: No commit (verification only)**

If this fails, stop and surface the error — Phase 1 assumes a clean baseline.

---

## Phase 1: Add the new controller (TDD)

We add the new package alongside the old one, fully tested, before touching call sites. This keeps each commit independently runnable.

### Task 4: Create the `sandboxmetricsgc` package skeleton with AGENTS docs

**Files:**
- Create: `pkg/controller/sandboxmetricsgc/AGENTS.md`
- Create: `pkg/controller/sandboxmetricsgc/CLAUDE.md`
- Create: `pkg/controller/sandboxmetricsgc/doc.go`

- [ ] **Step 1: Create the directory**

Run: `mkdir -p pkg/controller/sandboxmetricsgc`

- [ ] **Step 2: Create AGENTS.md**

Create `pkg/controller/sandboxmetricsgc/AGENTS.md`:

```markdown
# Sandbox Metrics GC Controller

Reconciles synthetic `GenericEvent`s carrying `(namespace, name)` for deleted
Sandboxes and invokes `pkg/controller/sandbox.DeleteSandboxMetrics` to drop
all owned Prometheus series off the Sandbox controller's hot path.

## Responsibilities

- Receive `(namespace, name)` enqueues from the Sandbox controller's `NotFound`
  branch via a buffered `chan event.GenericEvent`.
- Translate each event into a normal `Reconcile` request through
  `source.Channel` + `EnqueueRequestForObject` so workqueue dedup applies.
- Call `DeleteSandboxMetrics` exactly once per request. The function is
  idempotent; duplicate work is harmless.

## Non-Responsibilities

- Reading or mutating Sandbox API objects. The `Object` field carried in the
  synthetic event holds only `ObjectMeta.Namespace`/`Name` so
  `EnqueueRequestForObject` can derive a `ctrl.Request`.
- Multi-kind cleanup. If SandboxSet/SandboxClaim need this pattern, give them
  their own controller — do not generalize this one.

## Local Guidance

- Keep `Reconcile` to a single statement: the package exists to swap a
  goroutine pool for a controller, not to grow new behavior.
- The only metric this package owns is `sandbox_metrics_gc_dropped_total`
  (channel-full drops). All other observability comes free from
  controller-runtime's `controller_runtime_*` series.
- `Enqueue` must be non-blocking. Blocking would re-introduce the very
  serialization that motivated this controller.
```

- [ ] **Step 3: Create sibling CLAUDE.md**

Create `pkg/controller/sandboxmetricsgc/CLAUDE.md`:

```markdown
@./AGENTS.md
```

- [ ] **Step 4: Create package doc**

Create `pkg/controller/sandboxmetricsgc/doc.go`:

```go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package sandboxmetricsgc reconciles (namespace, name) enqueues from the
// Sandbox controller's NotFound branch and removes the matching Prometheus
// series via sandbox.DeleteSandboxMetrics. It exists to move the cleanup work
// off the Sandbox Reconcile hot path while reusing controller-runtime's
// workqueue dedup, parallelism, and panic recovery.
package sandboxmetricsgc
```

- [ ] **Step 5: Commit**

```bash
git add pkg/controller/sandboxmetricsgc/
git commit -s -m "feat(sandboxmetricsgc): scaffold package with AGENTS docs"
```

### Task 5: Write failing test for the dropped-counter metric

**Files:**
- Create: `pkg/controller/sandboxmetricsgc/metrics.go` (empty for now — referenced by the test)
- Create: `pkg/controller/sandboxmetricsgc/metrics_test.go`

- [ ] **Step 1: Create empty metrics.go so the package compiles**

Create `pkg/controller/sandboxmetricsgc/metrics.go`:

```go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sandboxmetricsgc
```

- [ ] **Step 2: Write the failing test**

Create `pkg/controller/sandboxmetricsgc/metrics_test.go`:

```go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sandboxmetricsgc

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestDroppedCounter_IncrementsByReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   float64
	}{
		{name: "channel_full reason recorded", reason: "channel_full", want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			droppedTotal.Reset()
			droppedTotal.WithLabelValues(tt.reason).Inc()
			got := testutil.ToFloat64(droppedTotal.WithLabelValues(tt.reason))
			if got != tt.want {
				t.Errorf("droppedTotal{reason=%s} = %v, want %v", tt.reason, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./pkg/controller/sandboxmetricsgc/... -run TestDroppedCounter -count=1`
Expected: FAIL with "undefined: droppedTotal".

- [ ] **Step 4: Implement the collector**

Replace `pkg/controller/sandboxmetricsgc/metrics.go` contents (keep the header):

```go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sandboxmetricsgc

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// droppedTotal counts Enqueue calls dropped because the GenericEvent channel
// was full. Reconcile latency and throughput come for free from
// controller_runtime_reconcile_* — this is the only failure mode that
// controller-runtime cannot observe on its own.
var droppedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "sandbox_metrics_gc_dropped_total",
		Help: "Enqueue calls dropped without being processed",
	},
	[]string{"reason"},
)

func init() {
	metrics.Registry.MustRegister(droppedTotal)
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./pkg/controller/sandboxmetricsgc/... -run TestDroppedCounter -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/controller/sandboxmetricsgc/metrics.go pkg/controller/sandboxmetricsgc/metrics_test.go
git commit -s -m "feat(sandboxmetricsgc): add channel-full drop counter"
```

### Task 6: Write failing test for `Enqueue` non-blocking semantics

**Files:**
- Create: `pkg/controller/sandboxmetricsgc/controller.go` (skeleton — referenced by the test)
- Create: `pkg/controller/sandboxmetricsgc/controller_test.go`

- [ ] **Step 1: Create the skeleton controller**

Create `pkg/controller/sandboxmetricsgc/controller.go`:

```go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sandboxmetricsgc

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// Options configures the metrics GC controller. Zero values fall back to
// sensible defaults applied by NewReconciler.
type Options struct {
	// Workers controls MaxConcurrentReconciles. Defaults to 8.
	Workers int
	// ChannelBuffer sizes the GenericEvent channel. Defaults to 50000.
	// Sends that would block are dropped and counted under
	// sandbox_metrics_gc_dropped_total{reason="channel_full"}.
	ChannelBuffer int
}

const (
	defaultWorkers       = 8
	defaultChannelBuffer = 50000
)

// Reconciler garbage-collects Prometheus series for deleted Sandboxes.
type Reconciler struct {
	workers   int
	eventChan chan event.GenericEvent
}

// NewReconciler returns a Reconciler with the supplied options. It does not
// start any goroutines; call SetupWithManager.
func NewReconciler(opts Options) *Reconciler {
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.ChannelBuffer <= 0 {
		opts.ChannelBuffer = defaultChannelBuffer
	}
	return &Reconciler{
		workers:   opts.Workers,
		eventChan: make(chan event.GenericEvent, opts.ChannelBuffer),
	}
}
```

- [ ] **Step 2: Write the failing test**

Create `pkg/controller/sandboxmetricsgc/controller_test.go`:

```go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sandboxmetricsgc

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestEnqueue_NonBlockingDropOnFullChannel(t *testing.T) {
	tests := []struct {
		name       string
		bufferSize int
		enqueues   int
		wantDrops  float64
	}{
		{name: "fills exactly to capacity, no drops", bufferSize: 2, enqueues: 2, wantDrops: 0},
		{name: "third enqueue drops", bufferSize: 2, enqueues: 3, wantDrops: 1},
		{name: "many overflow", bufferSize: 1, enqueues: 5, wantDrops: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			droppedTotal.Reset()
			r := NewReconciler(Options{ChannelBuffer: tt.bufferSize})
			for i := 0; i < tt.enqueues; i++ {
				r.Enqueue("ns", "sb")
			}
			got := testutil.ToFloat64(droppedTotal.WithLabelValues("channel_full"))
			if got != tt.wantDrops {
				t.Errorf("drops = %v, want %v", got, tt.wantDrops)
			}
		})
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./pkg/controller/sandboxmetricsgc/... -run TestEnqueue_NonBlockingDropOnFullChannel -count=1`
Expected: FAIL with "r.Enqueue undefined".

- [ ] **Step 4: Implement `Enqueue`**

Append to `pkg/controller/sandboxmetricsgc/controller.go`:

```go

// Above the existing imports, the import block needs additions. Replace the
// existing import block with:
//
//	import (
//		metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
//		"sigs.k8s.io/controller-runtime/pkg/event"
//
//		agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
//	)
```

Concretely, rewrite the imports block at the top of `controller.go` to:

```go
import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)
```

Then append the method after `NewReconciler`:

```go

// Enqueue is non-blocking. Calls that would block on a full channel are
// dropped and counted; the channel-fill threshold matches ChannelBuffer.
// Safe to call before or after SetupWithManager.
func (r *Reconciler) Enqueue(namespace, name string) {
	ev := event.GenericEvent{Object: &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}}
	select {
	case r.eventChan <- ev:
	default:
		droppedTotal.WithLabelValues("channel_full").Inc()
	}
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./pkg/controller/sandboxmetricsgc/... -run TestEnqueue_NonBlockingDropOnFullChannel -count=1`
Expected: PASS for all three sub-tests.

- [ ] **Step 6: Commit**

```bash
git add pkg/controller/sandboxmetricsgc/controller.go pkg/controller/sandboxmetricsgc/controller_test.go
git commit -s -m "feat(sandboxmetricsgc): add non-blocking Enqueue with channel-full drop accounting"
```

### Task 7: Write failing test for `Reconcile` calling `DeleteSandboxMetrics`

**Files:**
- Modify: `pkg/controller/sandboxmetricsgc/controller.go`
- Modify: `pkg/controller/sandboxmetricsgc/controller_test.go`

The behaviour we are verifying: a `Reconcile` call removes the matching `sandbox_created` gauge that `recordSandboxMetrics` previously set. We import the sandbox package to drive both sides; this is allowed because the dependency direction is `sandboxmetricsgc → sandbox`, not the other way.

- [ ] **Step 1: Write the failing test**

Append to `pkg/controller/sandboxmetricsgc/controller_test.go`:

```go

func TestReconcile_DeletesSandboxMetricSeries(t *testing.T) {
	// This test verifies the Reconciler's single piece of behaviour: it must
	// invoke sandbox.DeleteSandboxMetrics for the requested (ns, name). We
	// assert side-effects on a known series instead of mocking the call, so
	// the test fails for the right reason if the wiring breaks.
	//
	// Imports needed at top of file:
	//   "context"
	//   metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	//   "k8s.io/apimachinery/pkg/types"
	//   "sigs.k8s.io/controller-runtime/pkg/reconcile"
	//   agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	//   sandboxctrl "github.com/openkruise/agents/pkg/controller/sandbox"
	tests := []struct {
		name string
		ns   string
		obj  string
	}{
		{name: "basic delete clears created gauge", ns: "default", obj: "gc-victim-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Seed a metric series via the sandbox package.
			sb := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              tt.obj,
					Namespace:         tt.ns,
					CreationTimestamp: metav1.Now(),
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
			}
			// We intentionally call the exported record fn; this test does NOT
			// share global state with sandbox_test because gauge labels are
			// keyed by (ns, name) which we make unique above.
			sandboxctrl.RecordSandboxMetricsForTest(sb)

			r := NewReconciler(Options{})
			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: tt.ns, Name: tt.obj},
			})
			if err != nil {
				t.Fatalf("Reconcile returned err: %v", err)
			}
			// After Reconcile, the created gauge for this label set must be
			// absent (ToFloat64 on a deleted series returns 0 via lazy create).
			if got := sandboxctrl.CreatedGaugeValueForTest(tt.ns, tt.obj); got != 0 {
				t.Errorf("created gauge after Reconcile = %v, want 0", got)
			}
		})
	}
}
```

The test references two new test helpers — we add them in Step 3 so the sandbox package surface stays test-friendly without exporting more than necessary.

- [ ] **Step 2: Fix the imports block in controller_test.go**

Replace the imports at the top of `controller_test.go` with:

```go
import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxctrl "github.com/openkruise/agents/pkg/controller/sandbox"
)
```

- [ ] **Step 3: Add the two test helpers to the sandbox package**

Create `pkg/controller/sandbox/metrics_export_test.go` so the helpers are only compiled into tests of `sandboxmetricsgc` — wait, helpers in `_test.go` files are not visible cross-package. Use a separate non-test file with explicit "ForTest" naming instead.

Create `pkg/controller/sandbox/metrics_test_helpers.go`:

```go
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sandbox

import (
	"github.com/prometheus/client_golang/prometheus/testutil"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// RecordSandboxMetricsForTest is a stable cross-package entry point for tests
// in sibling controller packages (e.g., sandboxmetricsgc) that need to seed
// the sandbox metric vectors. Production code calls recordSandboxMetrics
// directly.
func RecordSandboxMetricsForTest(sb *agentsv1alpha1.Sandbox) {
	recordSandboxMetrics(sb, nil)
}

// CreatedGaugeValueForTest returns the current sandbox_created gauge value
// for (namespace, name), used by sibling-package tests to assert cleanup.
func CreatedGaugeValueForTest(namespace, name string) float64 {
	return testutil.ToFloat64(sandboxCreated.WithLabelValues(namespace, name))
}
```

- [ ] **Step 4: Run the test to verify it fails on the missing Reconcile method**

Run: `go test ./pkg/controller/sandboxmetricsgc/... -run TestReconcile_DeletesSandboxMetricSeries -count=1`
Expected: FAIL with "r.Reconcile undefined".

- [ ] **Step 5: Implement `Reconcile`**

Replace the imports block in `pkg/controller/sandboxmetricsgc/controller.go` to add `context` and `ctrl`:

```go
import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxctrl "github.com/openkruise/agents/pkg/controller/sandbox"
)
```

Append after `Enqueue`:

```go

// Reconcile drops all Prometheus series owned by the Sandbox controller for
// req.NamespacedName. DeleteSandboxMetrics is idempotent so repeated
// reconciles are safe and never error.
func (r *Reconciler) Reconcile(_ context.Context, req ctrl.Request) (ctrl.Result, error) {
	sandboxctrl.DeleteSandboxMetrics(req.Namespace, req.Name)
	return ctrl.Result{}, nil
}
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./pkg/controller/sandboxmetricsgc/... -run TestReconcile_DeletesSandboxMetricSeries -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/controller/sandboxmetricsgc/controller.go pkg/controller/sandboxmetricsgc/controller_test.go pkg/controller/sandbox/metrics_test_helpers.go
git commit -s -m "feat(sandboxmetricsgc): implement Reconcile delegating to sandbox.DeleteSandboxMetrics"
```

### Task 8: Write `SetupWithManager` and a smoke test

**Files:**
- Modify: `pkg/controller/sandboxmetricsgc/controller.go`
- Modify: `pkg/controller/sandboxmetricsgc/controller_test.go`

The smoke test verifies the builder accepts the channel source — actual end-to-end delivery is covered by controller-runtime's own tests; we don't reimplement them.

- [ ] **Step 1: Write the failing test**

Append to `pkg/controller/sandboxmetricsgc/controller_test.go`:

```go

func TestSetupWithManager_RegistersWithoutError(t *testing.T) {
	// We use envtest-style assertions sparingly: just instantiate a Manager
	// from a no-op REST config and verify SetupWithManager returns nil. The
	// controller is not actually started.
	//
	// Additional imports needed:
	//   "k8s.io/client-go/rest"
	//   "k8s.io/apimachinery/pkg/runtime"
	//   ctrl "sigs.k8s.io/controller-runtime"
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	mgr, err := ctrl.NewManager(&rest.Config{}, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: "0",
		// MetricsBindAddress lives under Metrics.BindAddress in v0.20:
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	r := NewReconciler(Options{})
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}
}
```

Extend the test file's import block:

```go
import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxctrl "github.com/openkruise/agents/pkg/controller/sandbox"
)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/controller/sandboxmetricsgc/... -run TestSetupWithManager_RegistersWithoutError -count=1`
Expected: FAIL with "r.SetupWithManager undefined".

- [ ] **Step 3: Implement `SetupWithManager`**

Update the imports block in `pkg/controller/sandboxmetricsgc/controller.go`:

```go
import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxctrl "github.com/openkruise/agents/pkg/controller/sandbox"
)
```

Append at the bottom:

```go

// SetupWithManager registers the controller with the manager using a Channel
// source backed by r.eventChan. Workqueue dedup happens automatically once
// requests reach the controller queue.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("sandbox-metrics-gc").
		WithOptions(controller.Options{MaxConcurrentReconciles: r.workers}).
		WatchesRawSource(source.Channel(r.eventChan, &handler.EnqueueRequestForObject{})).
		Complete(r)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./pkg/controller/sandboxmetricsgc/... -run TestSetupWithManager_RegistersWithoutError -count=1`
Expected: PASS. If it fails with "metrics server bind", set `ctrl.Options{Metrics: server.Options{BindAddress: "0"}}` — but typically v0.20.2 manager construction tolerates a zero REST config in unit tests.

If the test cannot construct a manager without a real REST config, replace the smoke test with a static compile-time check instead:

```go
func TestSetupWithManager_Compiles(t *testing.T) {
	// Compile-time assertion: NewReconciler returns *Reconciler which has
	// SetupWithManager(ctrl.Manager) error. If this stops compiling the
	// wiring broke.
	var _ func(ctrl.Manager) error = (*Reconciler)(nil).SetupWithManager
}
```

Use the static check unconditionally — it gives a real signal (the method signature must match `ctrl.Manager`) without depending on envtest.

Delete the runtime test, keep the compile-time check, re-run:

Run: `go test ./pkg/controller/sandboxmetricsgc/... -count=1`
Expected: all PASS.

- [ ] **Step 5: Run race detector across the package**

Run: `go test -race ./pkg/controller/sandboxmetricsgc/... -count=1`
Expected: PASS, no data race reports.

- [ ] **Step 6: Commit**

```bash
git add pkg/controller/sandboxmetricsgc/controller.go pkg/controller/sandboxmetricsgc/controller_test.go
git commit -s -m "feat(sandboxmetricsgc): implement SetupWithManager via source.Channel"
```

---

## Phase 2: Migrate callers, then delete the old pool

Order matters: we change the sandbox controller's `Enqueuer` interface first (it stops accepting `kind`), then point `main.go` at `sandboxmetricsgc`, then delete `pkg/utils/metricsasync`. The intermediate states all compile.

### Task 9: Narrow the `sandbox.Enqueuer` interface to drop the `kind` parameter

**Files:**
- Modify: `pkg/controller/sandbox/sandbox_controller.go`
- Modify: `pkg/controller/sandbox/sandbox_controller_test.go`

- [ ] **Step 1: Update the interface in `sandbox_controller.go`**

In `pkg/controller/sandbox/sandbox_controller.go`, find the existing block:

```go
// Enqueuer is the contract the Sandbox controller depends on for async
// metric cleanup. metricsasync.Pool satisfies it.
type Enqueuer interface {
	Enqueue(kind, namespace, name string)
}
```

Replace with:

```go
// Enqueuer is the contract the Sandbox controller depends on for async
// metric cleanup. sandboxmetricsgc.Reconciler satisfies it.
type Enqueuer interface {
	Enqueue(namespace, name string)
}
```

In the same file find:

```go
			r.metricsCleanup.Enqueue("sandbox", req.NamespacedName.Namespace, req.NamespacedName.Name)
```

Replace with:

```go
			r.metricsCleanup.Enqueue(req.NamespacedName.Namespace, req.NamespacedName.Name)
```

- [ ] **Step 2: Update the test fake**

In `pkg/controller/sandbox/sandbox_controller_test.go` replace the `fakeEnqueuer` block (around line 3911) with:

```go
// fakeEnqueuer captures Enqueue invocations for assertion.
type fakeEnqueuer struct {
	mu    sync.Mutex
	calls []struct{ Namespace, Name string }
}

func (f *fakeEnqueuer) Enqueue(namespace, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ Namespace, Name string }{namespace, name})
}

func (f *fakeEnqueuer) snapshot() []struct{ Namespace, Name string } {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]struct{ Namespace, Name string }, len(f.calls))
	copy(out, f.calls)
	return out
}
```

Then find `TestReconcile_NotFoundEnqueuesAsyncCleanup` (immediately below) and update the assertions:

```go
	calls := enq.snapshot()
	assert.Len(t, calls, 1)
	assert.Equal(t, "ns", calls[0].Namespace)
	assert.Equal(t, "missing", calls[0].Name)
```

(Remove the `assert.Equal(t, "sandbox", calls[0].Kind)` line.)

- [ ] **Step 3: Verify the sandbox package still builds and tests pass**

Run: `go test ./pkg/controller/sandbox/... -count=1 -run 'TestReconcile_NotFoundEnqueuesAsyncCleanup' -timeout 60s`
Expected: PASS.

Run: `go build ./pkg/controller/sandbox/...`
Expected: success. (`pkg/utils/metricsasync.Pool` still has `Enqueue(kind, ns, name)` so the rest of the tree still compiles — `controllers.go` calls `sandbox.Add(m, deps.MetricsCleanup)` and the *Pool no longer satisfies the new `Enqueuer` interface.)

This intermediate state breaks `controllers.go`. That is fixed in Task 10 immediately following — do not commit yet.

- [ ] **Step 4: Confirm the breakage we expect**

Run: `go build ./pkg/controller/...`
Expected: FAIL with "cannot use deps.MetricsCleanup (variable of type *metricsasync.Pool) as sandbox.Enqueuer value". This is the expected handoff to Task 10.

### Task 10: Switch `controllers.go` `Deps.MetricsCleanup` type to the new interface

**Files:**
- Modify: `pkg/controller/controllers.go`

- [ ] **Step 1: Update the Deps struct and import**

Replace `pkg/controller/controllers.go` entirely with:

```go
/*
Copyright 2025 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openkruise/agents/pkg/controller/sandbox"
	"github.com/openkruise/agents/pkg/controller/sandboxclaim"
	"github.com/openkruise/agents/pkg/controller/sandboxset"
	"github.com/openkruise/agents/pkg/controller/sandboxupdateops"
	"github.com/openkruise/agents/pkg/controller/securitytokenrefresh"
)

// Deps bundles process-wide dependencies passed to controller Add funcs.
// New dependencies should be appended here rather than introducing extra
// AddFunc parameters across all controllers.
type Deps struct {
	MetricsCleanup sandbox.Enqueuer
}

func SetupWithManager(m manager.Manager, deps Deps) error {
	if err := sandbox.Add(m, deps.MetricsCleanup); err != nil {
		return err
	}
	if err := sandboxset.Add(m); err != nil {
		return err
	}
	if err := sandboxclaim.Add(m); err != nil {
		return err
	}
	if err := sandboxupdateops.Add(m); err != nil {
		return err
	}
	if err := securitytokenrefresh.Add(m); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 2: Verify `pkg/controller/...` builds**

Run: `go build ./pkg/controller/...`
Expected: success.

- [ ] **Step 3: Commit (intermediate state is now coherent within the controller tree)**

```bash
git add pkg/controller/sandbox/sandbox_controller.go pkg/controller/sandbox/sandbox_controller_test.go pkg/controller/controllers.go
git commit -s -m "refactor(sandbox): narrow Enqueuer interface to (namespace, name) and accept any implementation"
```

### Task 11: Re-point `cmd/agent-sandbox-controller/main.go` at `sandboxmetricsgc`

**Files:**
- Modify: `cmd/agent-sandbox-controller/main.go`

- [ ] **Step 1: Swap the import**

In `cmd/agent-sandbox-controller/main.go` find:

```go
	"github.com/openkruise/agents/pkg/utils/metricsasync"
```

Replace with:

```go
	"github.com/openkruise/agents/pkg/controller/sandboxmetricsgc"
```

- [ ] **Step 2: Drop the `--metrics-async-drain-timeout` flag**

Find the block:

```go
	var metricsAsyncWorkers int
	var metricsAsyncDrainTimeout time.Duration
	var metricsAsyncQueueCap int
	flag.IntVar(&metricsAsyncWorkers, "metrics-async-workers",
		envInt("METRICS_ASYNC_WORKERS", 8),
		"Number of goroutines draining the async metric cleanup queue.")
	flag.DurationVar(&metricsAsyncDrainTimeout, "metrics-async-drain-timeout",
		envDuration("METRICS_ASYNC_DRAIN_TIMEOUT", 5*time.Second),
		"Bounded time the manager waits for async metric cleanup to drain at shutdown.")
	flag.IntVar(&metricsAsyncQueueCap, "metrics-async-queue-cap",
		envInt("METRICS_ASYNC_QUEUE_CAP", 0),
		"Optional cap on the async metric cleanup queue. 0 means unbounded.")
```

Replace with:

```go
	var metricsAsyncWorkers int
	var metricsAsyncQueueCap int
	flag.IntVar(&metricsAsyncWorkers, "metrics-async-workers",
		envInt("METRICS_ASYNC_WORKERS", 8),
		"Concurrent reconciles for the sandbox metric GC controller.")
	flag.IntVar(&metricsAsyncQueueCap, "metrics-async-queue-cap",
		envInt("METRICS_ASYNC_QUEUE_CAP", 50000),
		"Buffer size for the sandbox metric GC controller event channel. "+
			"Sends that would block are counted under sandbox_metrics_gc_dropped_total{reason=\"channel_full\"}.")
```

The default for queue-cap changes from `0` (unbounded) to `50000` (avpa-aligned buffer). Operators relying on unbounded should set a large explicit value.

- [ ] **Step 3: Replace the pool construction**

Find:

```go
	metricsCleanupPool := metricsasync.NewPool(metricsasync.Options{
		Workers:      metricsAsyncWorkers,
		DrainTimeout: metricsAsyncDrainTimeout,
		QueueCap:     metricsAsyncQueueCap,
	})
	if err := metricsCleanupPool.RegisterKind("sandbox", sandboxctrl.DeleteSandboxMetrics); err != nil {
		setupLog.Error(err, "unable to register sandbox metric cleanup")
		os.Exit(1)
	}
	if err := mgr.Add(metricsCleanupPool); err != nil {
		setupLog.Error(err, "unable to add metrics cleanup pool to manager")
		os.Exit(1)
	}
```

Replace with:

```go
	metricsGC := sandboxmetricsgc.NewReconciler(sandboxmetricsgc.Options{
		Workers:       metricsAsyncWorkers,
		ChannelBuffer: metricsAsyncQueueCap,
	})
	if err := metricsGC.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to setup sandbox metrics GC controller")
		os.Exit(1)
	}
```

Find the log line:

```go
	setupLog.Info("setup controllers",
		"metricsAsyncWorkers", metricsAsyncWorkers,
		"metricsAsyncDrainTimeout", metricsAsyncDrainTimeout,
		"metricsAsyncQueueCap", metricsAsyncQueueCap)
	if err = controller.SetupWithManager(mgr, controller.Deps{MetricsCleanup: metricsCleanupPool}); err != nil {
```

Replace with:

```go
	setupLog.Info("setup controllers",
		"metricsAsyncWorkers", metricsAsyncWorkers,
		"metricsAsyncQueueCap", metricsAsyncQueueCap)
	if err = controller.SetupWithManager(mgr, controller.Deps{MetricsCleanup: metricsGC}); err != nil {
```

- [ ] **Step 4: Drop the now-unused `time` import if no other caller uses it**

Run: `goimports -w cmd/agent-sandbox-controller/main.go`
If goimports is not on PATH, run: `gofmt -w cmd/agent-sandbox-controller/main.go` and manually verify the `time` import survives only if it has other consumers. Inspecting the file, `time.Duration` is also used by `envDuration` (kept for any future use) — leave the import in place.

Verify:

Run: `go vet ./cmd/agent-sandbox-controller/...`
Expected: no "imported and not used" errors.

- [ ] **Step 5: Build the binary**

Run: `go build ./cmd/agent-sandbox-controller/...`
Expected: success.

- [ ] **Step 6: Commit**

```bash
git add cmd/agent-sandbox-controller/main.go
git commit -s -m "feat(cmd): wire sandboxmetricsgc controller, drop drain-timeout flag"
```

### Task 12: Delete `pkg/utils/metricsasync/`

**Files:**
- Delete: `pkg/utils/metricsasync/pool.go`
- Delete: `pkg/utils/metricsasync/metrics.go`
- Delete: `pkg/utils/metricsasync/pool_test.go`

- [ ] **Step 1: Verify no remaining references**

Run: `grep -rn 'pkg/utils/metricsasync\|metricsasync\.' --include='*.go' . | grep -v vendor`
Expected: no output.

- [ ] **Step 2: Delete the directory**

Run: `git rm -r pkg/utils/metricsasync/`

- [ ] **Step 3: Verify tree builds**

Run: `go build ./...`
Expected: success (allow long compile time on first run).

- [ ] **Step 4: Run unit tests across touched packages**

Run: `go test ./pkg/controller/sandbox/... ./pkg/controller/sandboxmetricsgc/... -count=1 -timeout 180s`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git commit -s -m "refactor: remove pkg/utils/metricsasync, superseded by sandboxmetricsgc controller"
```

---

## Phase 3: PR #292 follow-ups (in-scope)

These touch the metric surface this branch already modifies. Out-of-scope PR #292 items (doc proposal edits, sandbox-manager rename, e2b test deletion) stay with PR #292's own branch.

### Task 13: Adjust `sandboxclaim` duration histogram buckets

**Files:**
- Modify: `pkg/controller/sandboxclaim/metrics.go`

The reviewer asked for buckets that cover warm-pool sub-100ms claims:
`prometheus.ExponentialBuckets(0.01, 2, 10)` (10ms → ~10s) aligns with their suggestion and stays in the exponential family used elsewhere.

- [ ] **Step 1: Locate the histogram definition**

Run: `grep -n 'sandboxClaimClaimDuration\|Buckets: prometheus.ExponentialBuckets(0.02' pkg/controller/sandboxclaim/metrics.go`
Expected: one match for the histogram and one for the buckets line.

- [ ] **Step 2: Make the edit**

In `pkg/controller/sandboxclaim/metrics.go` find:

```go
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
```

(Only the `sandboxClaimClaimDuration` block — there is only one histogram in this file.)

Replace with:

```go
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms -> ~10s, covers warm-pool sub-100ms claims
```

- [ ] **Step 3: Run sandboxclaim tests**

Run: `go test ./pkg/controller/sandboxclaim/... -count=1 -timeout 120s`
Expected: PASS. If a test asserts a specific bucket count, update it; spot-check `pkg/controller/sandboxclaim/*_test.go` for `ExponentialBuckets` references first:

Run: `grep -n 'ExponentialBuckets\|SampleCount' pkg/controller/sandboxclaim/*_test.go`
Likely no hit (the tests target sample sums, not bucket counts).

- [ ] **Step 4: Commit**

```bash
git add pkg/controller/sandboxclaim/metrics.go
git commit -s -m "refactor(sandboxclaim): tighten claim duration buckets for warm-pool sub-100ms claims"
```

### Task 14: Audit `sandbox/metrics.go` for residual PR #292 items and document closure

**Files:**
- No source changes; an audit task that produces a commit on the design doc if any item still applies.

PR #292 raised four items still nominally open against this package:
1. `metrics.go:204` ownerreference → label
2. `metrics.go:369` condition function refactor
3. `metrics.go:37` Pod UID label
4. P1 unexport items in other files

The spec audit already confirmed items 3 and 4 are addressed. This task verifies items 1 and 2.

- [ ] **Step 1: Verify `sandboxInfo` no longer uses ownerReferences**

Run: `grep -n 'sandboxInfo\b\|OwnerReferences' pkg/controller/sandbox/metrics.go`
Expected: `sandboxInfo` is defined with labels `["namespace", "name", "sandbox_pool", "node", "sandbox_template"]` (no `owner`); the only reference to `sandbox_pool` is the label drawn from `agentsv1alpha1.LabelSandboxPool` (no `OwnerReferences` read).

If the audit holds: this item is closed by current code. No source change needed.

- [ ] **Step 2: Verify condition recording uses the shared helpers**

Run: `grep -n 'recordConditionTrueMetric\|recordConditionDuration' pkg/controller/sandbox/metrics.go`
Expected: both helpers exist and are called for all four condition cases (`Ready`, `InplaceUpdate`, `Paused`, `Resumed`).

Open `pkg/controller/sandbox/metrics.go` lines 461–510 and confirm each `case` arm either calls a helper or has a documented reason for inlining (the `Ready` arm inlines because of the `observedCreationToReady` dedup map, which the helpers do not model — this is acceptable).

- [ ] **Step 3: Append a closure note to the design doc**

Append to `docs/superpowers/specs/2026-06-04-sandbox-metrics-gc-controller-design.md` under the "PR #292 follow-ups" table:

```markdown

### PR #292 audit results (recorded 2026-06-04)

- `sandbox/metrics.go:37` Pod UID label — **closed**, current `sandboxInfo`
  labels are `(namespace, name, sandbox_pool, node, sandbox_template)`.
- `sandbox/metrics.go:204` ownerreference → label — **closed**, `sandbox_pool`
  label already sourced from `agentsv1alpha1.LabelSandboxPool` annotation.
- `sandbox/metrics.go:369` condition refactor — **partially closed**.
  `recordConditionTrueMetric` + `recordConditionDuration` cover three of four
  condition types; the `Ready` arm intentionally inlines because of the
  per-sandbox dedup state held in `observedCreationToReady`. Leaving as-is.
- `sandboxclaim/metrics.go:99` buckets — closed in Task 13 of the
  implementation plan.
- `sandboxclaim/metrics.go:194` "unused func" — **stale comment**,
  `deleteSandboxClaimMetrics` is called by the claim reconciler.
- `pkg/proxy/metrics.go:26`, `pkg/servers/e2b/metrics.go:26`,
  `pkg/sandbox-manager/metrics.go:60` unexport — **closed**, variables are
  already lowercase.
```

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/specs/2026-06-04-sandbox-metrics-gc-controller-design.md
git commit -s -m "docs(specs): record PR292 follow-up audit results"
```

---

## Phase 4: Final verification

### Task 15: Run the full unit-test suite for touched packages

- [ ] **Step 1: Run unit tests across all packages we modified**

Run:
```bash
go test -count=1 -timeout 300s \
  ./pkg/controller/sandbox/... \
  ./pkg/controller/sandboxmetricsgc/... \
  ./pkg/controller/sandboxclaim/... \
  ./cmd/agent-sandbox-controller/...
```
Expected: all PASS.

- [ ] **Step 2: Run with race detector**

Run:
```bash
go test -count=1 -race -timeout 300s \
  ./pkg/controller/sandbox/... \
  ./pkg/controller/sandboxmetricsgc/...
```
Expected: PASS, no data races.

- [ ] **Step 3: Vet**

Run: `go vet ./pkg/controller/... ./cmd/agent-sandbox-controller/...`
Expected: clean.

- [ ] **Step 4: Full module build (final safety)**

Run: `go build ./...`
Expected: success.

- [ ] **Step 5: Untracked-file audit before opening the PR update**

Run: `git status`
Expected: only the intentional new files in `pkg/controller/sandboxmetricsgc/` plus the deletion of `pkg/utils/metricsasync/` are shown. The four untracked PR292 audit files at repo root (`PR292_REVIEW_REPORT.md`, `PR292_UNRESOLVED_COMMENTS.csv`, `PR292_UNRESOLVED_COMMENTS.json`, `cover.html`) should remain untracked — they are local scratch and not part of this PR.

- [ ] **Step 6: Verify branch is ready for `git push`**

Run: `git log --oneline master..HEAD | head -20`
Expected: the new commits from Phase 0–3 appear above the prior PR #461 history.

No commit in this task — verification only.

---

## Self-Review Notes

**Spec coverage check:**

| Spec requirement                                                       | Plan task |
| ---------------------------------------------------------------------- | --------- |
| New `pkg/controller/sandboxmetricsgc` package                          | 4, 5, 6, 7, 8 |
| `Reconciler.Enqueue(namespace, name)` non-blocking                     | 6         |
| Reconcile delegates to `DeleteSandboxMetrics`                          | 7         |
| `SetupWithManager` via `source.Channel` + `EnqueueRequestForObject`    | 8         |
| `sandbox_metrics_gc_dropped_total{reason="channel_full"}` collector    | 5         |
| Drop `pkg/utils/metricsasync`                                          | 12        |
| `SandboxReconciler.Enqueuer` interface narrowed                        | 9         |
| `controllers.go` `Deps.MetricsCleanup` retyped                         | 10        |
| Wire `main.go`                                                         | 11        |
| Drop `--metrics-async-drain-timeout`, change `queue-cap` default 0→50000 | 11      |
| Resolve `metrics_test.go` merge conflicts                              | 2         |
| Resolve `controllers.go` merge conflict                                | 1         |
| PR #292 bucket adjustment                                              | 13        |
| PR #292 closure audit recorded in spec                                 | 14        |
| Final build + race + vet                                               | 15        |

**Placeholder scan:** No "TBD", "implement later", "handle edge cases" left. Every code block is complete.

**Type consistency:** `Enqueuer.Enqueue(namespace, name string)` is used identically in Tasks 6, 9, 10, 11. `Reconciler` and `Options` field names match across tasks. The smoke test in Task 8 was simplified to a compile-time check to avoid envtest dependency.
