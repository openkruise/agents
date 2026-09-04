//go:build auto

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

// It orchestrates the whole load-test environment (kind + kwok + agents + pool)
//  and runs the load test, by shelling out to docker/kind/kubectl/go.
//
//	cd test/kwok_loadtest
//	go run -tags auto .
//
// There are NO command-line flags/subcommands: orchestration parameters come
// from params/main_params.json, load-test parameters from params/generator_params.json.
// The pool size is whatever loadtest-sandboxset.yaml declares.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents-api/agents/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// orch holds the orchestration settings, read from params/main_params.json.
// kindBin is not from config.
type orch struct {
	Cluster                string  `json:"cluster"`
	KwokVersion            string  `json:"kwok_version"`
	KindVersion            string  `json:"kind_version"`
	NS                     string  `json:"ns"`
	KwokRepo               string  `json:"kwok_repo"`
	PoolFillThreshold      float64 `json:"pool_fill_threshold"`       // fraction of spec.replicas considered "filled", see setupPool
	PoolFillTimeoutSeconds int     `json:"pool_fill_timeout_seconds"` // hard cap on waiting for the pool to fill, see setupPool
	kindBin                string
}

// loadOrch starts from the defaults, then unmarshals params/main_params.json ON TOP.
func loadOrch(path string) *orch {
	o := &orch{
		Cluster:                "kind",
		KwokVersion:            "v0.8.0",
		KindVersion:            "v0.24.0",
		NS:                     "sandbox-system",
		KwokRepo:               "kubernetes-sigs/kwok",
		PoolFillThreshold:      0.995,
		PoolFillTimeoutSeconds: 3600,
		kindBin:                "kind",
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return o
	}
	if err := json.Unmarshal(raw, o); err != nil {
		fmt.Fprintf(os.Stderr, "parse config %s: %v\n", path, err)
		os.Exit(2)
	}
	return o
}

func logf(format string, a ...any) { fmt.Printf("\n\033[1;34m==> "+format+"\033[0m\n", a...) }

func run(dir, name string, args ...string) {
	fmt.Printf("+ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "command failed: %s %v: %v\n", name, args, err)
		os.Exit(1)
	}
}

// tryRun runs a command but ignores errors (for best-effort cleanup, like `|| true`).
func tryRun(name string, args ...string) {
	_ = exec.Command(name, args...).Run()
}

// output captures a command's trimmed stdout (errors ignored).
func output(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out))
}

func main() {
	// Preflight: check hard dependencies before doing any real work.
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintln(os.Stderr, "error: docker not found — install Docker and ensure it is on PATH")
		os.Exit(1)
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "error: docker daemon not running — start Docker\n%s\n", out)
		os.Exit(1)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		fmt.Fprintln(os.Stderr, "error: kubectl not found — install kubectl and ensure it is on PATH")
		os.Exit(1)
	}

	// Run from test/kwok_loadtest (`cd test/kwok_loadtest && go run -tags auto .`): read the
	// current directory as the load-test dir; the repo root is two levels up.
	ltDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "getwd: %v\n", err)
		os.Exit(1)
	}
	root := filepath.Dir(filepath.Dir(ltDir))
	o := loadOrch(filepath.Join(ltDir, "params", "main_params.json"))

	ensureKind(o)
	ensureCluster(o, root)
	installKwok(o, ltDir)
	deployAgents(o, root)
	setupPool(o, ltDir)
	runLoadtest(o, ltDir)
	down(o, root)
}

// ensureKind makes sure the kind binary is available, installing it via
// `go install` if missing (go is a base prerequisite). Sets o.kindBin to the
// resolved path when it installs one.
func ensureKind(o *orch) {
	if _, err := exec.LookPath("kind"); err == nil {
		return
	}
	logf("kind not found — installing sigs.k8s.io/kind@%s via go install", o.KindVersion)
	run(".", "go", "install", "sigs.k8s.io/kind@"+o.KindVersion)
	binDir := output("go", "env", "GOBIN")
	if binDir == "" {
		binDir = filepath.Join(output("go", "env", "GOPATH"), "bin")
	}
	o.kindBin = filepath.Join(binDir, "kind")
	if _, err := os.Stat(o.kindBin); err != nil {
		fmt.Fprintf(os.Stderr, "kind install did not produce %s\n", o.kindBin)
		os.Exit(1)
	}
}

func ensureCluster(o *orch, root string) {
	if contains(strings.Fields(output(o.kindBin, "get", "clusters")), o.Cluster) {
		logf("kind cluster %q already exists", o.Cluster)
	} else {
		logf("creating kind cluster %q", o.Cluster)
		run(".", o.kindBin, "create", "cluster", "--name", o.Cluster,
			"--config", filepath.Join(root, "test", "kwok_loadtest", "manifests", "kind-conf-loadtest.yaml"))
	}
	run(".", "kubectl", "config", "use-context", "kind-"+o.Cluster)

	// Raise kindnet's resource limits before deploying anything else
	logf("raising kindnet resource limits for 100k-pod scale")
	run(".", "kubectl", "-n", "kube-system", "patch", "daemonset", "kindnet", "--type", "json",
		"--patch-file", filepath.Join(root, "test", "kwok_loadtest", "manifests", "kindnet-resources-patch.yaml"))
	run(".", "kubectl", "-n", "kube-system", "rollout", "status", "daemonset/kindnet", "--timeout=60s")
}

func installKwok(o *orch, ltDir string) {
	// Detect: skip the controller install if it's already deployed (fast re-runs).
	if output("kubectl", "-n", "kube-system", "get", "deploy", "kwok-controller",
		"-o", "name", "--ignore-not-found") != "" {
		logf("kwok-controller already installed — skipping")
	} else {
		logf("installing kwok controller + default stages (%s)", o.KwokVersion)
		base := fmt.Sprintf("https://github.com/%s/releases/download/%s", o.KwokRepo, o.KwokVersion)
		run(".", "kubectl", "apply", "-f", base+"/kwok.yaml")
		run(".", "kubectl", "apply", "-f", base+"/stage-fast.yaml")
		run(".", "kubectl", "-n", "kube-system", "rollout", "status", "deploy/kwok-controller", "--timeout=120s")
	}

	// Fake node + timing stages: apply is idempotent, so always (re)apply.
	logf("applying fake node + timing stages")
	run(".", "kubectl", "apply", "-f", filepath.Join(ltDir, "manifests", "fake-node.yaml"))
	run(".", "kubectl", "apply", "-f", filepath.Join(ltDir, "manifests", "kwok-timing-stages.yaml"))
}

func deployAgents(o *orch, root string) {
	// Always rebuild + reload the images so the run tests the CURRENT code
	logf("building + loading agents images into kind")
	run(root, "docker", "build", "-f", "dockerfiles/agent-sandbox-controller.Dockerfile",
		"-t", "agent-sandbox-controller:latest", ".")
	run(root, "docker", "build", "-f", "dockerfiles/sandbox-manager.Dockerfile",
		"-t", "sandbox-manager:latest", ".")
	run(".", o.kindBin, "load", "docker-image", "--name", o.Cluster, "agent-sandbox-controller:latest")
	run(".", o.kindBin, "load", "docker-image", "--name", o.Cluster, "sandbox-manager:latest")

	// Ensure kustomize is available in {root}/bin/; install via go install if missing.
	kustomize := filepath.Join(root, "bin", "kustomize")
	if _, err := os.Stat(kustomize); err != nil {
		logf("kustomize not found — installing sigs.k8s.io/kustomize/kustomize/v5@v5.6.0")
		cmd := exec.Command("go", "install", "sigs.k8s.io/kustomize/kustomize/v5@v5.6.0")
		cmd.Env = append(os.Environ(), "GOBIN="+filepath.Join(root, "bin"))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "kustomize install failed: %v\n", err)
			os.Exit(1)
		}
	}

	logf("deploying CRDs + controller + manager")
	run(root, "sh", "-c", kustomize+" build config/crd | kubectl apply -f -")
	run(root, "sh", "-c", kustomize+" build test/kwok_loadtest/manifests/kustomize-overlay/manager | kubectl apply -f -")
	run(root, "sh", "-c", kustomize+" build test/kwok_loadtest/manifests/kustomize-overlay/sandbox-manager | kubectl apply -f -")

	logf("waiting for agents components to be ready")
	run(".", "kubectl", "-n", o.NS, "rollout", "status", "deploy/sandbox-controller-manager", "--timeout=180s")
	run(".", "kubectl", "-n", o.NS, "rollout", "status", "deploy/sandbox-manager", "--timeout=180s")
}

func setupPool(o *orch, ltDir string) {
	logf("applying the loadtest SandboxSet")
	run(".", "kubectl", "apply", "-f", filepath.Join(ltDir, "manifests", "loadtest-sandboxset.yaml"))

	kc, err := buildSandboxSetClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build client for pool polling: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	key := ctrlclient.ObjectKey{Namespace: "default", Name: "loadtest"}

	var sbs agentsv1alpha1.SandboxSet
	if err := kc.Get(ctx, key, &sbs); err != nil {
		fmt.Fprintf(os.Stderr, "failed to read loadtest SandboxSet: %v\n", err)
		os.Exit(1)
	}
	want := int(sbs.Spec.Replicas)
	// kwok-timing-stages.yaml deliberately fails ~0.1% of pods (pod-ready-fail) to simulate
	// ImagePullBackOff, which never become available — so avail never actually reaches want.
	// Accept o.PoolFillThreshold (default 99.5%) as "filled" instead, and keep the loop's
	// outer bound (o.PoolFillTimeoutSeconds, default 1h) as a hard safety cap.
	threshold := want
	if t := int(float64(want) * o.PoolFillThreshold); t < threshold {
		threshold = t
	}
	const pollInterval = 2 * time.Second
	logf("waiting for pool to become available (%d, up to %s)", want, time.Duration(o.PoolFillTimeoutSeconds)*time.Second)
	filled := false
	for deadline := time.Now().Add(time.Duration(o.PoolFillTimeoutSeconds) * time.Second); time.Now().Before(deadline); {
		if err := kc.Get(ctx, key, &sbs); err == nil {
			if avail := int(sbs.Status.AvailableReplicas); avail >= threshold {
				fmt.Printf("pool ready: %d/%d\n", avail, want)
				filled = true
				break
			}
		}
		time.Sleep(pollInterval)
	}
	if !filled {
		fmt.Println("warning: pool did not fully fill; continuing anyway")
	}
}

// buildSandboxSetClient builds a controller-runtime client.Client from the current
// kube context
func buildSandboxSetClient() (ctrlclient.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return ctrlclient.New(restCfg, ctrlclient.Options{Scheme: scheme})
}

func runLoadtest(o *orch, ltDir string) {
	logf("port-forwarding svc/sandbox-manager 7788")
	pf := exec.Command("kubectl", "port-forward", "svc/sandbox-manager", "-n", o.NS, "7788:7788")
	if err := pf.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "port-forward failed: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = pf.Process.Kill() }()
	time.Sleep(2 * time.Second)

	logf("running loadtest (params from params/generator_params.json)")
	run(ltDir, "go", "run", "-tags", "jsonrun", ".")
}

// tear the environment down once the test finishes
func down(o *orch, root string) {
	logf("undeploying agents")
	kustomize := filepath.Join(root, "bin", "kustomize")
	tryRun("sh", "-c", kustomize+" build "+filepath.Join(root, "test/kwok_loadtest/manifests/kustomize-overlay/sandbox-manager")+" | kubectl delete -f -")
	tryRun("sh", "-c", kustomize+" build "+filepath.Join(root, "config/undeploy")+" | kubectl delete -f -")
	logf("deleting kind cluster %q", o.Cluster)
	tryRun(o.kindBin, "delete", "cluster", "--name", o.Cluster)

	// deployAgents() always rebuilds both images from scratch (see its comment), so
	// BuildKit's cache grows unbounded across reruns — 40GB+ after a day of testing.
	logf("pruning docker build cache")
	tryRun("docker", "builder", "prune", "-f")
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
