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

package main

// Simplified Kwok load test: open-loop claim from the pool via the sandbox SDK,
// with optional in-place update and pause/resume.
//
// Simplest possible variant:
//   - NO SandboxClaim CR path (claim via the E2B/manager HTTP API only).
//   - NO config file — every parameter is a fixed constant below; edit and re-run.
//
//	cd test/loadtest
//	go run ./sample
//
// Prerequisites: port-forward svc/sandbox-manager 7788, the pool scaled with
// headroom, and (for realistic timing) kwok-timing-stages.yaml applied.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/openkruise/agents-api/sdk/sandbox"
)

// ── Fixed parameters (edit here instead of a config file) ──
const (
	apiURL   = "http://localhost:7788/kruise/api"
	apiKey   = "some-api-key"
	template = "loadtest" // pool name (the SandboxSet)

	rate        = 5.0              // arrival rate: claims per second
	duration    = 15 * time.Second // total run time
	maxInflight = 20               // max concurrent in-flight claims

	inplaceImage = "busybox:1.36" // image used for the in-place update

	reqTimeout = 30 * time.Second // per-request timeout
	outDir     = "results"

	// SDK metadata keys.
	skipInitRuntimeKey = "e2b.agents.kruise.io/skip-init-runtime"
	inplaceImageKey    = "e2b.agents.kruise.io/image"
)

const sboxTimeout int32 = 60 // sandbox TTL (seconds)

type sample struct {
	op  string
	lat time.Duration
	ok  bool
}

func main() {
	connOpts := []sandbox.ConnectionConfigOption{
		sandbox.WithAPIURL(apiURL),
		sandbox.WithAPIKey(apiKey),
		sandbox.WithRequestTimeout(reqTimeout),
	}

	// One collector goroutine drains samples so workers never block on I/O.
	samplesCh := make(chan sample, 1<<16)
	var all []sample
	var collectWg sync.WaitGroup
	collectWg.Add(1)
	go func() {
		defer collectWg.Done()
		for s := range samplesCh {
			all = append(all, s)
		}
	}()
	rec := func(op string, lat time.Duration, ok bool) { samplesCh <- sample{op, lat, ok} }

	t0 := time.Now()
	runOpen(connOpts, rec, t0.Add(duration))
	close(samplesCh)
	collectWg.Wait()
	report(all, time.Since(t0))
}

// fireOnce runs the full fixed sequence for one request: claim (with in-place
// update) -> pause -> resume. No randomness — every request does all three.
// Each op records its own latency/result.
func fireOnce(connOpts []sandbox.ConnectionConfigOption, rec func(string, time.Duration, bool), scheduled time.Time) {
	sb, err := sandbox.Create(context.Background(), template,
		sandbox.WithTimeout(sboxTimeout),
		sandbox.WithMetadata(map[string]string{
			skipInitRuntimeKey: "true",
			inplaceImageKey:    inplaceImage, // claim carries an in-place image update
		}),
		sandbox.WithConfig(connOpts...),
	)
	rec("claim_update", time.Since(scheduled), err == nil)
	if err != nil {
		return
	}
	defer func() { _, _ = sb.Kill(context.Background()) }()

	t := time.Now()
	if _, err := sb.Pause(context.Background()); err != nil {
		rec("pause", time.Since(t), false)
		return
	}
	rec("pause", time.Since(t), true)

	t = time.Now()
	if _, err := sandbox.Connect(context.Background(), sb.SandboxID(), sandbox.WithConfig(connOpts...)); err != nil {
		rec("resume", time.Since(t), false)
		return
	}
	rec("resume", time.Since(t), true)
}

// runOpen fires at a fixed arrival rate (open-loop): it does NOT wait for the
// previous claim to return before firing the next. maxInflight bounds concurrency.
func runOpen(connOpts []sandbox.ConnectionConfigOption, rec func(string, time.Duration, bool), deadline time.Time) {
	interval := time.Duration(float64(time.Second) / rate)
	sem := make(chan struct{}, maxInflight)
	var wg sync.WaitGroup
	next := time.Now()

	for time.Now().Before(deadline) {
		sem <- struct{}{}
		wg.Add(1)
		scheduled := next
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fireOnce(connOpts, rec, scheduled)
		}()
		next = next.Add(interval)
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
	}
	wg.Wait()
}

func pct(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Round(q / 100 * float64(len(sorted)-1)))
	if idx > len(sorted)-1 {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
func round2(f float64) float64   { return math.Round(f*100) / 100 }

type opStats struct {
	Op             string  `json:"op"`
	OK             int     `json:"ok"`
	Fail           int     `json:"fail"`
	SuccessPct     float64 `json:"success_pct"`
	ThroughputOpsS float64 `json:"throughput_ops_s"`
	AvgMs          float64 `json:"avg_ms"`
	P50Ms          float64 `json:"p50_ms"`
	P95Ms          float64 `json:"p95_ms"`
	P99Ms          float64 `json:"p99_ms"`
	MaxMs          float64 `json:"max_ms"`
}

// report aggregates per-op latency into percentiles, prints a compact summary,
// and writes the full result as JSON under outDir/.
func report(all []sample, wall time.Duration) {
	okLat := map[string][]time.Duration{}
	fail := map[string]int{}
	for _, s := range all {
		if s.ok {
			okLat[s.op] = append(okLat[s.op], s.lat)
		} else {
			fail[s.op]++
		}
	}

	var ops []opStats
	for _, op := range []string{"claim_update", "pause", "resume"} {
		lats := okLat[op]
		if len(lats) == 0 && fail[op] == 0 {
			continue
		}
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		okN, failN := len(lats), fail[op]
		total := okN + failN
		succ := 0.0
		if total > 0 {
			succ = 100 * float64(okN) / float64(total)
		}
		tput := float64(okN) / duration.Seconds()
		avg, mx := 0.0, 0.0
		if okN > 0 {
			var sum time.Duration
			for _, d := range lats {
				sum += d
			}
			avg = ms(sum) / float64(okN)
			mx = ms(lats[okN-1])
		}
		ops = append(ops, opStats{
			Op: op, OK: okN, Fail: failN,
			SuccessPct:     round2(succ),
			ThroughputOpsS: round2(tput),
			AvgMs:          round2(avg),
			P50Ms:          round2(ms(pct(lats, 50))),
			P95Ms:          round2(ms(pct(lats, 95))),
			P99Ms:          round2(ms(pct(lats, 99))),
			MaxMs:          round2(mx),
		})
	}

	stamp := time.Now().Format("20060102-150405")
	doc := map[string]any{
		"timestamp":            stamp,
		"rate":       rate,
		"duration_s": duration.Seconds(),
		"wall_s":               round2(wall.Seconds()),
		"total_samples":        len(all),
		"ops":                  ops,
	}

	// Compact stdout summary.
	for _, o := range ops {
		fmt.Printf("%-13s ok=%-4d fail=%-3d succ=%5.1f%%  p50=%.0fms p95=%.0fms p99=%.0fms  tput=%.2f/s\n",
			o.Op, o.OK, o.Fail, o.SuccessPct, o.P50Ms, o.P95Ms, o.P99Ms, o.ThroughputOpsS)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir results: %v\n", err)
	}
	path := filepath.Join(outDir, fmt.Sprintf("loadtest-sample-%s.json", stamp))
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal result: %v\n", err)
		return
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write result: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "results -> "+path)
}
