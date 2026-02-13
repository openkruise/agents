package main

import (
	"context"
	"net/http"
	"os"
	"strconv"

	"k8s.io/klog/v2"

	flagOptions "github.com/openkruise/agents/cmd/agent-runtime/options"
	agent_runtime "github.com/openkruise/agents/pkg/agent-runtime"
	"github.com/openkruise/agents/pkg/agent-runtime/host"
	"github.com/openkruise/agents/pkg/agent-runtime/logs"
	"github.com/openkruise/agents/pkg/agent-runtime/openapi/types"
	"github.com/openkruise/agents/pkg/utils"
	utilMap "github.com/openkruise/agents/pkg/utils/map"
)

const (
	// This is the default user used in the container if not specified otherwise.
	// It should be always overridden by the user in /init when building the template.
	defaultUser = "root"
)

/*
Main function entry point for the sandboxRuntime,
serving as a standard service interface process that provides compatibility with e2b and envd environments.
It extends beyond basic compatibility by offering enhanced interface capabilities, including storage mounting functionality through CSI plugins.
*/
func main() {
	klog.InitFlags(nil)
	flagOptions.InitFlagOptions()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logContext := logs.NewLoggerContext(ctx, agent_runtime.SandboxRuntimeHttpServer, agent_runtime.SandboxRuntimeHttpServerVersion)
	logCollector := klog.FromContext(logContext)

	if err := os.MkdirAll(host.E2BRunDir, 0o755); err != nil {
		logCollector.Error(err, "error creating E2B run directory")
	}

	// To print the version
	if flagOptions.VersionFlag {
		logCollector.V(3).Info(agent_runtime.SandboxRuntimeHttpServer, agent_runtime.SandboxRuntimeHttpServerVersion)
	}

	// Start pprof server if enabled
	if flagOptions.EnablePprof {
		go func() {
			klog.Info("starting pprof server", "addr", flagOptions.PprofAddr)
			if err := http.ListenAndServe(flagOptions.PprofAddr, nil); err != nil {
				logCollector.Error(err, "unable to start pprof server")
			}
		}()
	}

	defaults := &types.Defaults{
		User:    defaultUser,
		EnvVars: utilMap.NewMap[string, string](),
	}

	// To set the default env var
	isFCBoolStr := strconv.FormatBool(!flagOptions.IsNotFC)
	defaults.EnvVars.Store("E2B_SANDBOX", isFCBoolStr)

	config := agent_runtime.ServerConfig{
		Port:      flagOptions.ServerPort,
		Workspace: flagOptions.Workspace,
		AuthConfig: agent_runtime.AuthConfig{
			ValidTokens:   utils.StringToSlice(flagOptions.ValidTokens, ","),
			AllowedPaths:  utils.StringToSlice(flagOptions.AllowedPaths, ","),
			EnableSigning: flagOptions.EnableSigning,
		},
		FlagConfig: agent_runtime.FlagConfig{
			VersionFlag:  flagOptions.VersionFlag,
			StartCmdFlag: flagOptions.StartCmdFlag,
		},
		Defaults: defaults,
	}

	// To config the cmd execution process
	if config.FlagConfig.StartCmdFlag != "" {
		// TODO: add the cmd execution process
	}

	// Create and start server and start
	if err := agent_runtime.NewHttpServer(config).Run(); err != nil {
		logCollector.Error(err, "Failed to start sandboxRuntime")
		os.Exit(1)
	}
	logCollector.Info("SandboxRuntime http stopped")
}
