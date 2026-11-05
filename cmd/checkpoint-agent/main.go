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

// Command checkpoint-agent runs the node-local executor of the Checkpoint API.
// It is deployed as a DaemonSet and executes only the Checkpoints whose target
// Pod runs on its own node, so it holds no leader election: every instance
// owns a disjoint set of Pods.
package main

import (
	"flag"
	"os"
	"strings"
	_ "time/tzdata" // Embed timezone database for scratch-based container images

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // Import all Kubernetes client auth plugins (Azure, GCP, OIDC, etc.)
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/checkpointagent"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))
}

func main() {
	var opts checkpointagent.Options
	var runtimeClasses string
	var probeAddr string
	var metricsAddr string

	flag.StringVar(&opts.NodeName, "node-name", os.Getenv("NODE_NAME"),
		"The node this agent runs on. Defaults to the NODE_NAME environment variable, "+
			"which the DaemonSet populates from spec.nodeName.")
	flag.StringVar(&opts.CRIEndpoint, "cri-endpoint", "unix:///run/containerd/containerd.sock",
		"The unix socket of the node's container runtime.")
	flag.StringVar(&opts.CheckpointRoot, "checkpoint-root", checkpointagent.DefaultCheckpointRoot,
		"The node-local directory the agent writes checkpoint artifacts into. It must be the same "+
			"path inside the container and on the host, because the path is handed to the runtime.")
	flag.StringVar(&runtimeClasses, "runtime-classes", "",
		"Comma-separated list of RuntimeClass names whose Pods may be checkpointed. "+
			"Empty allows every Pod on the node, which is only safe when every runtime on the node "+
			"implements the CRI CheckpointPod RPC.")
	flag.DurationVar(&opts.CheckpointTimeout, "checkpoint-timeout", checkpointagent.DefaultCheckpointTimeout,
		"Bounds a single CheckpointPod RPC.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0",
		"The address the metrics endpoint binds to, or 0 to disable it.")

	zapOpts := zap.Options{Development: true}
	zapOpts.BindFlags(flag.CommandLine)
	klog.InitFlags(nil)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	for _, rc := range strings.Split(runtimeClasses, ",") {
		if rc = strings.TrimSpace(rc); rc != "" {
			opts.RuntimeClasses = append(opts.RuntimeClasses, rc)
		}
	}

	if opts.NodeName == "" {
		setupLog.Error(nil, "node name is empty: set --node-name or the NODE_NAME environment variable")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		// No leader election: each agent owns the Pods of its own node, so
		// every instance has to run.
		LeaderElection: false,
		Cache: ctrlcache.Options{
			ByObject: map[ctrlclient.Object]ctrlcache.ByObject{
				// Restricting the Pod cache to this node is what keeps a
				// per-node DaemonSet from holding the whole cluster's Pods,
				// and it is also how the reconciler decides ownership: a
				// Checkpoint whose Pod is absent here belongs to another node.
				&corev1.Pod{}: {
					Field: fields.OneTermEqualSelector("spec.nodeName", opts.NodeName),
				},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := checkpointagent.Add(mgr, opts); err != nil {
		setupLog.Error(err, "unable to set up the node checkpoint agent")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting node checkpoint agent",
		"node", opts.NodeName, "criEndpoint", opts.CRIEndpoint,
		"checkpointRoot", opts.CheckpointRoot, "runtimeClasses", opts.RuntimeClasses,
		"checkpointTimeout", opts.CheckpointTimeout.String())
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
