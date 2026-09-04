//go:build jsonrun

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

// Claim / pause / resume go through E2B SDK;
// the optional SandboxClaim-CR sub-path uses
// client-go directly against the apiserver.
//
// All parameters are read from a JSON config file (default ./params/generator_params.json),
// giving a fixed, reproducible operating point.
//
//	cd test/kwok_loadtest
//	go run -tags jsonrun .        # reads ./params/generator_params.json
//	go run -tags jsonrun . my.json

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents-api/agents/v1alpha1"
	"github.com/openkruise/agents-api/sdk/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// skipInitRuntimeKey tells the manager to skip runtime (envd) init.
	skipInitRuntimeKey = "e2b.agents.kruise.io/skip-init-runtime"
	// inplaceImageKey, when present, makes the claim also trigger an in-place
	// image update (ExtensionKeyClaimWithImage in pkg/servers/e2b/models).
	inplaceImageKey = "e2b.agents.kruise.io/image"
	// crLabelKey tags the SandboxClaim and its claimed sandboxes so the CR
	// sub-path can clean them up.
	crLabelKey = "loadtest-cr"
)

type jsonConfig struct {
	APIURL             string  `json:"apiurl"`
	APIKey             string  `json:"apikey"`
	Template           string  `json:"template"`
	InplaceUpdateRatio float64 `json:"inplace_update_ratio"`
	InplaceImage       string  `json:"inplace_image"`
	PauseResumeRatio   float64 `json:"pause_resume_ratio"`
	Concurrency        int     `json:"concurrency"` // number of worker goroutines
	DurationSeconds    int     `json:"duration_seconds"`
	SandboxTimeoutSec  int     `json:"sandbox_timeout_seconds"`
	ReqTimeoutSeconds  int     `json:"req_timeout_seconds"`
	OutDir             string  `json:"out"`
	CRClaim            bool    `json:"cr_claim"`
	CRNamespace        string  `json:"cr_namespace"`
	CRReplicas         int     `json:"cr_replicas"`
	CRCount            int     `json:"cr_count"`
	CRTimeoutSeconds   int     `json:"cr_timeout_seconds"`
	CRDelaySeconds     int     `json:"cr_delay_seconds"`
	SlowestIDsCount    int     `json:"slowest_ids_count"` // per-op count of slowest request IDs to report, see opStats.SlowestIDs
}

type config struct {
	apiURL             string
	apiKey             string
	template           string
	inplaceUpdateRatio float64
	inplaceImage       string
	pauseResumeRatio   float64
	concurrency        int
	duration           time.Duration
	sboxTimeout        int32
	reqTimeout         time.Duration
	outDir             string
	crClaim            bool
	crNamespace        string
	crReplicas         int
	crCount            int
	crTimeout          time.Duration
	crDelay            time.Duration
	slowestIDsCount    int
}

func loadJSONConfig(path string) (config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	var j jsonConfig
	if err := json.Unmarshal(raw, &j); err != nil {
		return config{}, err
	}
	orStr := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	orInt := func(v, def int) int {
		if v == 0 {
			return def
		}
		return v
	}
	return config{
		apiURL:             orStr(j.APIURL, "http://localhost:7788/kruise/api"),
		apiKey:             orStr(j.APIKey, "some-api-key"),
		template:           orStr(j.Template, "loadtest"),
		inplaceUpdateRatio: j.InplaceUpdateRatio,
		inplaceImage:       orStr(j.InplaceImage, "busybox:1.36"),
		pauseResumeRatio:   j.PauseResumeRatio,
		concurrency:        orInt(j.Concurrency, 100),
		duration:           time.Duration(orInt(j.DurationSeconds, 60)) * time.Second,
		sboxTimeout:        int32(orInt(j.SandboxTimeoutSec, 180)),
		reqTimeout:         time.Duration(orInt(j.ReqTimeoutSeconds, 45)) * time.Second,
		outDir:             orStr(j.OutDir, "results"),
		crClaim:            j.CRClaim,
		crNamespace:        orStr(j.CRNamespace, "default"),
		crReplicas:         orInt(j.CRReplicas, 100),
		crCount:            orInt(j.CRCount, 2),
		crTimeout:          time.Duration(orInt(j.CRTimeoutSeconds, 180)) * time.Second,
		crDelay:            time.Duration(j.CRDelaySeconds) * time.Second,
		slowestIDsCount:    orInt(j.SlowestIDsCount, 10),
	}, nil
}

type sample struct {
	op     string
	lat    time.Duration
	ok     bool
	id     string // request ID
	errMsg string // non-empty only when !ok — the actual client-side error.
}

// genRequestID returns a 32-hex-char ID matching the manager's X-Request-ID /
// OTel-TraceID format, so a slow/failed sample's ID can be grepped directly in the
// manager/controller logs.
func genRequestID() string {
	b := make([]byte, 16)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}

// withHeader is a custom sdk/sandbox.ConnectionConfigOption: the SDK exposes
// no WithHeader helper, but ConnectionConfigOption is just a func(*ConnectionConfig)
// so building in this way is enough to set X-Request-ID.
func withHeader(key, value string) sandbox.ConnectionConfigOption {
	return func(c *sandbox.ConnectionConfig) {
		if c.Headers == nil {
			c.Headers = make(map[string]string)
		}
		c.Headers[key] = value
	}
}

func main() {
	path := "params/generator_params.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	cfg, err := loadJSONConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config %s: %v\n", path, err)
		os.Exit(2)
	}

	if cfg.concurrency <= 0 {
		fmt.Fprintln(os.Stderr, "concurrency must be > 0")
		os.Exit(2)
	}
	if cfg.pauseResumeRatio < 0 || cfg.pauseResumeRatio > 1 {
		fmt.Fprintln(os.Stderr, "pause_resume_ratio must be in [0,1]")
		os.Exit(2)
	}
	if cfg.inplaceUpdateRatio < 0 || cfg.inplaceUpdateRatio > 1 {
		fmt.Fprintln(os.Stderr, "inplace_update_ratio must be in [0,1]")
		os.Exit(2)
	}

	// The CR sub-path talks to the apiserver via client-go.
	var kc client.Client
	if cfg.crClaim {
		kc, err = buildClient()
		if err != nil {
			fmt.Fprintf(os.Stderr, "build kube client: %v\n", err)
			os.Exit(1)
		}
	}

	connOpts := []sandbox.ConnectionConfigOption{
		sandbox.WithAPIURL(cfg.apiURL),
		sandbox.WithAPIKey(cfg.apiKey),
		sandbox.WithRequestTimeout(cfg.reqTimeout),
	}

	// One channel for all data collected; report separates CR ops from E2B ops by name.
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
	rec := func(op string, lat time.Duration, ok bool, id, errMsg string) {
		samplesCh <- sample{op, lat, ok, id, errMsg}
	}

	t0 := time.Now()
	deadline := t0.Add(cfg.duration)
	runClosed(kc, connOpts, cfg, rec, deadline)
	close(samplesCh)
	collectWg.Wait()
	report(cfg, all, time.Since(t0))
}

// runClosed drives a closed-loop E2B load: cfg.concurrency worker goroutines,
// each firing a claim, waiting for it to finish, then immediately firing the next.
// The CR sub-path runs concurrently.
func runClosed(kc client.Client, connOpts []sandbox.ConnectionConfigOption, cfg config, rec func(string, time.Duration, bool, string, string), deadline time.Time) {
	crDone := make(chan struct{})
	if cfg.crClaim {
		go func() {
			defer close(crDone)
			runStamp := time.Now().Format("150405")
			if cfg.crDelay > 0 {
				time.Sleep(cfg.crDelay)
			}
			// cfg.crCount iterations; each does a no-update claim then an update
			// claim, looped sequentially in this single goroutine.
			for i := 0; i < cfg.crCount; i++ {
				for _, withUpdate := range []bool{false, true} {
					fireCRClaim(kc, cfg, rec, withUpdate, runStamp, i)
				}
			}
		}()
	} else {
		close(crDone)
	}

	var wg sync.WaitGroup
	for w := 0; w < cfg.concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				crFinished := false
				select {
				case <-crDone:
					crFinished = true
				default:
				}
				if time.Now().After(deadline) && crFinished {
					return
				}
				fireOnce(connOpts, cfg, rec, time.Now())
			}
		}()
	}
	wg.Wait()
}

func buildMeta(withUpdate bool, image string) map[string]string {
	m := map[string]string{skipInitRuntimeKey: "true"}
	if withUpdate {
		m[inplaceImageKey] = image
	}
	return m
}

func doClaim(ctx context.Context, connOpts []sandbox.ConnectionConfigOption, cfg config, meta map[string]string, requestID string) (*sandbox.Sandbox, error) {
	return sandbox.Create(ctx, cfg.template,
		sandbox.WithTimeout(cfg.sboxTimeout),
		sandbox.WithMetadata(meta),
		sandbox.WithConfig(connOpts...),
		sandbox.WithConfig(withHeader("X-Request-ID", requestID)),
	)
}

// fireOnce fires one claim (optionally inplace-update, pause+resume)
// all sharing a single request ID: the Sandbox returned by doClaim
// carries the header on its own stored client, but sandbox.Connect
// builds a fresh config, so the ID must be passed again.
func fireOnce(connOpts []sandbox.ConnectionConfigOption, cfg config, rec func(string, time.Duration, bool, string, string), scheduled time.Time) {
	withUpdate := rand.Float64() < cfg.inplaceUpdateRatio
	claimOp := "claim"
	if withUpdate {
		claimOp = "claim_update"
	}
	id := genRequestID()
	sb, err := doClaim(context.Background(), connOpts, cfg, buildMeta(withUpdate, cfg.inplaceImage), id)
	if err != nil {
		rec(claimOp, time.Since(scheduled), false, id, err.Error())
		return
	}
	rec(claimOp, time.Since(scheduled), true, id, "")
	defer func() { _, _ = sb.Kill(context.Background()) }()

	if rand.Float64() < cfg.pauseResumeRatio {
		t := time.Now()
		if _, err := sb.Pause(context.Background()); err != nil {
			rec("pause", time.Since(t), false, id, err.Error())
			return
		}
		rec("pause", time.Since(t), true, id, "")

		t = time.Now()
		if _, err := sandbox.Connect(context.Background(), sb.SandboxID(),
			sandbox.WithConfig(connOpts...), sandbox.WithConfig(withHeader("X-Request-ID", id))); err != nil {
			rec("resume", time.Since(t), false, id, err.Error())
			return
		}
		rec("resume", time.Since(t), true, id, "")
	}
}

// buildClient builds a controller-runtime client.Client from the current kube
// context, with the agents CRD types registered. Used only by the CR sub-path.
func buildClient() (client.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}
	restCfg.QPS = 500
	restCfg.Burst = 1000
	scheme := runtime.NewScheme()
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))
	return client.New(restCfg, client.Options{Scheme: scheme})
}

// newSandboxClaim builds the SandboxClaim object for the CR sub-path
func newSandboxClaim(name string, cfg config, withUpdate bool) *agentsv1alpha1.SandboxClaim {
	claim := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.crNamespace,
			Labels:    map[string]string{crLabelKey: name},
		},
		Spec: agentsv1alpha1.SandboxClaimSpec{
			TemplateName:    cfg.template,
			Replicas:        ptr.To(int32(cfg.crReplicas)),
			SkipInitRuntime: true,
			Labels:          map[string]string{crLabelKey: name}, // propagated to claimed sandboxes
		},
	}
	if withUpdate {
		claim.Spec.InplaceUpdate = &agentsv1alpha1.SandboxClaimInplaceUpdateOptions{Image: cfg.inplaceImage}
	}
	return claim
}

// fireCRClaim performs ONE SandboxClaim create → wait-completed → cleanup via
// client-go (clone.go style), recording a single result.
//
// This path bypasses sandbox-manager's HTTP layer entirely (kc.Create goes
// straight to the apiserver), and pkg/controller/sandboxclaim does not import
// pkg/tracing, so there is no X-Request-ID / traceID to attach here.
func fireCRClaim(kc client.Client, cfg config, rec func(string, time.Duration, bool, string, string), withUpdate bool, runStamp string, idx int) {
	ctx := context.Background()
	name := fmt.Sprintf("lt-cr-%s-%d-%t", runStamp, idx, withUpdate)
	claim := newSandboxClaim(name, cfg, withUpdate)
	op := "cr_claim"
	if withUpdate {
		op = "cr_claim_update"
	}
	t := time.Now()
	if err := kc.Create(ctx, claim); err != nil {
		rec(op, time.Since(t), false, "", err.Error())
		return
	}
	ok := waitClaimCompleted(ctx, kc, cfg, name, cfg.crTimeout)
	errMsg := ""
	if !ok {
		errMsg = "timed out waiting for claim to reach Completed phase"
	}
	rec(op, time.Since(t), ok, "", errMsg)
	// Best-effort cleanup: delete the claim and any sandboxes it claimed.
	_ = kc.Delete(ctx, claim)
	_ = kc.DeleteAllOf(ctx, &agentsv1alpha1.Sandbox{},
		client.InNamespace(cfg.crNamespace), client.MatchingLabels{crLabelKey: name})
}

func waitClaimCompleted(ctx context.Context, kc client.Client, cfg config, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	key := types.NamespacedName{Namespace: cfg.crNamespace, Name: name}
	for time.Now().Before(deadline) {
		var claim agentsv1alpha1.SandboxClaim
		if err := kc.Get(ctx, key, &claim); err == nil && claim.Status.Phase == agentsv1alpha1.SandboxClaimPhaseCompleted {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
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

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// crRun is one CR-claim execution's individual result.
type crRun struct {
	Op        string  `json:"op"`
	LatencyMs float64 `json:"latency_ms"`
	OK        bool    `json:"ok"`
	Error     string  `json:"error,omitempty"`
}

// failureDetail records one failed sample's request/trace ID alongside the
// actual client-side error, so a failure is diagnosable straight from the
// result JSON instead of requiring a dig through server logs.
type failureDetail struct {
	ID    string `json:"id,omitempty"`
	Error string `json:"error"`
}

type opStats struct {
	Op             string          `json:"op"`
	OK             int             `json:"ok"`
	Fail           int             `json:"fail"`
	SuccessPct     float64         `json:"success_pct"`
	ThroughputOpsS float64         `json:"throughput_ops_s"`
	AvgMs          float64         `json:"avg_ms"`
	P50Ms          float64         `json:"p50_ms"`
	P95Ms          float64         `json:"p95_ms"`
	P99Ms          float64         `json:"p99_ms"`
	MaxMs          float64         `json:"max_ms"`
	SlowestIDs     []string        `json:"slowest_ids,omitempty"` // request IDs of the cfg.slowestIDsCount slowest, slowest first
	Failures       []failureDetail `json:"failures,omitempty"`
}

// latSample pairs a latency with the request/trace ID that produced it, so
// the slowest samples can still be identified after sorting by latency.
type latSample struct {
	lat time.Duration
	id  string
}

func report(cfg config, all []sample, wall time.Duration) {
	okLat := map[string][]latSample{}
	fail := map[string]int{}
	failDetails := map[string][]failureDetail{}
	var crRuns []crRun
	for _, s := range all {
		if s.op == "cr_claim" || s.op == "cr_claim_update" {
			crRuns = append(crRuns, crRun{Op: s.op, LatencyMs: round2(ms(s.lat)), OK: s.ok, Error: s.errMsg})
			continue
		}
		if s.ok {
			okLat[s.op] = append(okLat[s.op], latSample{s.lat, s.id})
		} else {
			fail[s.op]++
			failDetails[s.op] = append(failDetails[s.op], failureDetail{ID: s.id, Error: s.errMsg})
		}
	}

	// E2B ops are aggregated to percentiles; CR claims are reported per execution.
	var ops []opStats
	for _, op := range []string{"claim", "claim_update", "pause", "resume"} {
		samples := okLat[op]
		if len(samples) == 0 && fail[op] == 0 {
			continue
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i].lat < samples[j].lat })
		lats := make([]time.Duration, len(samples))
		for i, s := range samples {
			lats[i] = s.lat
		}
		okN, failN := len(lats), fail[op]
		total := okN + failN
		succ := 0.0
		if total > 0 {
			succ = 100 * float64(okN) / float64(total)
		}
		tput := 0.0
		if cfg.duration.Seconds() > 0 {
			tput = float64(okN) / cfg.duration.Seconds()
		}
		var avg float64
		if okN > 0 {
			var sum time.Duration
			for _, d := range lats {
				sum += d
			}
			avg = ms(sum) / float64(okN)
		}
		var mx float64
		if okN > 0 {
			mx = ms(lats[okN-1])
		}
		var slowestIDs []string
		if okN > 0 {
			k := min(cfg.slowestIDsCount, okN)
			for i := okN - 1; i >= okN-k; i-- {
				slowestIDs = append(slowestIDs, samples[i].id)
			}
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
			SlowestIDs:     slowestIDs,
			Failures:       failDetails[op],
		})
	}

	stamp := time.Now().Format("20060102-150405")
	doc := map[string]any{
		"timestamp":     stamp,
		"wall_s":        round2(wall.Seconds()),
		"total_samples": len(all),
		// Key params/generator_params.json parameters that produced this run (for reproducibility).
		"config": map[string]any{
			"template":             cfg.template,
			"concurrency":          cfg.concurrency,
			"duration_s":           cfg.duration.Seconds(),
			"inplace_update_ratio": cfg.inplaceUpdateRatio,
			"pause_resume_ratio":   cfg.pauseResumeRatio,
			"inplace_image":        cfg.inplaceImage,
			"sandbox_timeout_s":    cfg.sboxTimeout,
			"req_timeout_s":        cfg.reqTimeout.Seconds(),
			"cr_claim":             cfg.crClaim,
			"cr_namespace":         cfg.crNamespace,
			"cr_replicas":          cfg.crReplicas,
			"cr_count":             cfg.crCount,
			"cr_timeout_s":         cfg.crTimeout.Seconds(),
		},
		"ops": ops,
	}
	if len(crRuns) > 0 {
		doc["cr_runs"] = crRuns // per-execution CR results, not aggregated
	}

	if err := os.MkdirAll(cfg.outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir results: %v\n", err)
	}
	path := filepath.Join(cfg.outDir, fmt.Sprintf("loadtest-closed-iu%.2f-pr%.2f-%s.json",
		cfg.inplaceUpdateRatio, cfg.pauseResumeRatio, stamp))
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
