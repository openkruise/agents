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

import (
	"flag"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/configstore"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/controller"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/credential"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/tokencache"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins/block"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins/bypass"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins/tokeninjection"
	"github.com/openkruise/agents/pkg/traffix-extension/runnable"
	runserver "github.com/openkruise/agents/pkg/traffix-extension/server"
)

var (
	grpcPort = flag.Int(
		"grpc-port",
		9002,
		"The gRPC port used for communicating with Envoy proxy")
	grpcHealthPort = flag.Int(
		"grpc-health-port",
		9003,
		"The port used for gRPC liveness and readiness probes")
	metricsPort = flag.Int(
		"metrics-port", 9090, "The metrics port")
	authMetrics = flag.Bool(
		"auth-metrics", false, "Enables authentication and authorization for metrics endpoint")
	streaming = flag.Bool(
		"streaming", false, "Enables streaming support for Envoy full-duplex streaming mode")
	logVerbosity = flag.Int("v", 2, "number for the log level verbosity")

	setupLog = ctrl.Log.WithName("setup")
)

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	initLogging(&opts)

	flags := make(map[string]any)
	flag.VisitAll(func(f *flag.Flag) {
		flags[f.Name] = f.Value.String()
	})
	setupLog.Info("Parsed flags", "flags", flags)

	// Init runtime.
	cfg, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "Failed to get rest config")
		return err
	}

	// Register metrics handler.
	metricsServerOptions := metricsserver.Options{
		BindAddress: fmt.Sprintf(":%d", *metricsPort),
	}
	if *authMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Metrics: metricsServerOptions,
	})
	if err != nil {
		setupLog.Error(err, "Failed to create manager", "config", cfg)
		return err
	}

	// Register our CRD types into the manager's scheme.
	utilruntime.Must(v1alpha1.AddToScheme(mgr.GetScheme()))

	ctx := ctrl.SetupSignalHandler()

	// Create in-memory config store.
	store := configstore.NewStore()

	// Register reconcilers.
	if err := (&controller.SecurityProfileReconciler{
		Reader: mgr.GetClient(),
		Store:  store,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to setup SecurityProfileReconciler")
		return err
	}

	// Register health server.
	if err := registerHealthServer(mgr, ctrl.Log.WithName("health"), *grpcHealthPort); err != nil {
		return err
	}

	// Setup ext-proc server runner.
	serverRunner := runserver.NewDefaultExtProcServerRunner(*grpcPort, *streaming)
	serverRunner.SecureServing = false // Disable TLS - ext-proc runs within the mesh
	serverRunner.ConfigStore = store

	// Create token cache and credential client.
	tokenCache := tokencache.NewCacheFromEnv()
	setupLog.Info("Token cache configured", "config", tokencache.ConfigInfo())
	credClient := credential.NewClientWithCache(tokenCache)
	serverRunner.CredClient = credClient

	// Register request-handling plugins. Order matters: Bypass runs first so
	// a matching Bypass rule short-circuits the chain unmodified before any
	// other plugin can act; Block runs next so a terminal Block action
	// short-circuits before any token mutation.
	serverRunner.Plugins = []plugins.Plugin{
		bypass.New(),
		block.New(),
		tokeninjection.New(credClient),
	}

	// Register ext-proc server.
	if err := mgr.Add(serverRunner.AsRunnable(ctrl.Log.WithName("ext-proc"))); err != nil {
		setupLog.Error(err, "Failed to register ext-proc gRPC server")
		return err
	}

	// Start the manager. This blocks until a signal is received.
	setupLog.Info("Manager starting")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Error starting manager")
		return err
	}
	setupLog.Info("Manager terminated")
	return nil
}

// registerHealthServer adds the Health gRPC server as a Runnable to the given manager.
func registerHealthServer(mgr manager.Manager, logger logr.Logger, port int) error {
	srv := grpc.NewServer()
	healthPb.RegisterHealthServer(srv, &healthServer{
		logger: logger,
	})
	if err := mgr.Add(
		runnable.NoLeaderElection(runnable.GRPCServer("health", srv, port))); err != nil {
		setupLog.Error(err, "Failed to register health server")
		return err
	}
	return nil
}

func initLogging(opts *zap.Options) {
	useV := true
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "zap-log-level" {
			useV = false
		}
	})
	if useV {
		// See https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/log/zap#Options.Level
		lvl := -1 * (*logVerbosity)
		opts.Level = uberzap.NewAtomicLevelAt(zapcore.Level(int8(lvl)))
	}

	logger := zap.New(zap.UseFlagOptions(opts), zap.RawZapOpts(uberzap.AddCaller()))
	ctrl.SetLogger(logger)
}
