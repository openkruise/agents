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

// Command runtime-probe checks the data-plane service running in the business
// container. It is a static Kubernetes exec-probe binary and needs no shell or
// diagnostic tools from the workload image.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/openkruise/agents/pkg/agent-runtime/runtimeprobe"
)

const (
	defaultEnvdAddress     = "127.0.0.1:49983"
	defaultHelperSocket    = "/var/run/agent-helper/helper.sock"
	defaultRuntimeStateDir = "/tmp/agent-runtime"
	probeModeFileName      = "runtime-probe-mode"
	defaultProbeTimeout    = 2 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		return writeUsage(stderr)
	}

	var err error
	switch args[0] {
	case "auto":
		err = runAutoProbe(args[1:], stderr)
	case "envd", "local":
		err = runEnvdProbe(args[1:], stderr)
	case "helper":
		err = runHelperProbe(args[1:], stderr)
	default:
		return writeUsage(stderr)
	}
	if err == nil {
		return 0
	}
	if _, writeErr := fmt.Fprintf(stderr, "runtime probe failed: %v\n", err); writeErr != nil {
		return 1
	}
	return 1
}

func runAutoProbe(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("auto", flag.ContinueOnError)
	flags.SetOutput(stderr)
	modeFile := flags.String("mode-file", defaultModeFilePath(), "file containing the mode and endpoint")
	timeout := flags.Duration("timeout", defaultProbeTimeout, "probe timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("auto probe does not accept positional arguments")
	}
	modeBytes, err := os.ReadFile(*modeFile)
	if err != nil {
		return fmt.Errorf("read data-plane mode file %q: %w", *modeFile, err)
	}
	lines := strings.Split(strings.TrimSpace(string(modeBytes)), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[1]) == "" {
		return fmt.Errorf("invalid data-plane probe configuration in %s", *modeFile)
	}
	mode := strings.TrimSpace(lines[0])
	endpoint := strings.TrimSpace(lines[1])
	switch mode {
	case "envd", "local":
		return runtimeprobe.ProbeEnvd(context.Background(), endpoint, *timeout)
	case "helper":
		return runtimeprobe.ProbeHelper(context.Background(), endpoint, *timeout)
	default:
		return fmt.Errorf("unsupported data-plane mode %q in %s", mode, *modeFile)
	}
}

func defaultModeFilePath() string {
	stateDir := os.Getenv("RUNTIME_STATE_DIR")
	if stateDir == "" {
		if tmpDir := os.Getenv("TMPDIR"); tmpDir != "" {
			stateDir = tmpDir + "/agent-runtime"
		} else {
			stateDir = defaultRuntimeStateDir
		}
	}
	return stateDir + "/" + probeModeFileName
}

func runEnvdProbe(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("envd", flag.ContinueOnError)
	flags.SetOutput(stderr)
	address := flags.String("address", defaultEnvdAddress, "envd TCP listen address")
	timeout := flags.Duration("timeout", defaultProbeTimeout, "probe timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("envd probe does not accept positional arguments")
	}
	return runtimeprobe.ProbeEnvd(context.Background(), *address, *timeout)
}

func runHelperProbe(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("helper", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", defaultHelperSocket, "agent-helper UNIX socket path")
	timeout := flags.Duration("timeout", defaultProbeTimeout, "probe timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("helper probe does not accept positional arguments")
	}
	return runtimeprobe.ProbeHelper(context.Background(), *socketPath, *timeout)
}

func writeUsage(w io.Writer) int {
	if _, err := fmt.Fprintln(w, "Usage: runtime-probe <auto|envd|helper|local> [flags]"); err != nil {
		return 1
	}
	return 2
}
