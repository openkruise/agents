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
	"context"
	"crypto/md5" // #nosec G501 -- non-security short hash
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/proto"

	"github.com/openkruise/agents/pkg/agent-runtime/mountpath"
	"github.com/openkruise/agents/pkg/agent-runtime/storage-cli/link"
	"github.com/openkruise/agents/pkg/agent-runtime/storage-cli/mountfinder"
	"github.com/openkruise/agents/pkg/agent-runtime/storage-cli/storage"
	"github.com/openkruise/agents/pkg/utils/logs"
	"github.com/spf13/cobra"
)

var (
	driver              string // driver name
	config              string // mount configuration for the chosen storage driver
	mountName           string // name of the shared mount-root volume; defaults to "mount-root"
	debugMode           bool   // when true, sensitive fields such as PublishContext are included in log output
	symlinkTargetPath   string
	symlinkLinkPath     string
	mountPathToValidate string
)

func init() {
	rootCmd.PersistentFlags().StringVarP(&driver, "driver", "d", "", "driver name for nas or oss or customfuse storage")
	rootCmd.PersistentFlags().StringVarP(&config, "config", "c", "", "(base64) specified storage mount config for nas or oss")
	rootCmd.PersistentFlags().StringVarP(&mountName, "mount-name", "m", "mount-root", "name of the shared mount-root volume used to locate the real mount path")
	rootCmd.PersistentFlags().BoolVar(&debugMode, "debug", false, "include sensitive fields (e.g. PublishContext) in log output; use only in non-production environments")
	symlinkCmd.Flags().StringVar(&symlinkTargetPath, "target", "", "absolute CSI mount target the symlink points to")
	symlinkCmd.Flags().StringVar(&symlinkLinkPath, "link", "", "absolute user-visible path where the symlink is created")
	validateMountPathCmd.Flags().StringVar(&mountPathToValidate, "path", "", "absolute user-visible mount path to validate")
}

var version = "unknown" // set via -ldflags at build time

var rootCmd = &cobra.Command{
	Use:   "sandbox-storage",
	Short: "A CLI tool for storage nas or oss mount/unmount for sandbox runtime",
	Run:   rootRun,
}

var mountCmd = &cobra.Command{
	Use:   "mount",
	Short: "Mount storage (NAS or OSS) to specified path",
	Run:   mountRun,
}

var symlinkCmd = &cobra.Command{
	Use:   "symlink",
	Short: "Create a user-visible symlink to a CSI mount",
	RunE: func(_ *cobra.Command, _ []string) error {
		return runSymlink(symlinkTargetPath, symlinkLinkPath)
	},
}

var validateMountPathCmd = &cobra.Command{
	Use:   "validate-mount-path",
	Short: "Validate a user-visible CSI mount path",
	RunE: func(_ *cobra.Command, _ []string) error {
		return runValidateMountPath(mountPathToValidate)
	},
}

func runSymlink(targetPath, linkPath string) error {
	return link.CreateSymlink(targetPath, linkPath)
}

func runValidateMountPath(pathValue string) error {
	return mountpath.Validate(pathValue, os.Getenv("PATH"))
}

// Injectable indirection variables for external dependencies.
// Tests in the same package may replace these to inject fakes;
// production code MUST NOT reassign them.
var (
	mountFinderFn   = mountfinder.FindMountPath
	storageLookupFn = storage.Lookup
	createSymlinkFn = link.CreateSymlink
)

func rootRun(cmd *cobra.Command, args []string) {
	cmd.Help() // #nosec G104 -- help output error is non-actionable
}

// runMount contains the core logic of the mount subcommand. It returns an
// error instead of calling os.Exit so that it can be exercised directly by
// unit tests. mountRun is the thin cobra handler that calls os.Exit on error.
func runMount(cmd *cobra.Command) error {
	configRaw, err := base64.StdEncoding.DecodeString(config)
	if err != nil {
		cmd.Help() // #nosec G104 -- help output error is non-actionable
		return fmt.Errorf("failed to decode CSI request config: %w", err)
	}

	csiReq := &csi.NodePublishVolumeRequest{}
	if err := proto.Unmarshal(configRaw, csiReq); err != nil {
		cmd.Help() // #nosec G104 -- help output error is non-actionable
		return fmt.Errorf("failed to unmarshal CSI request: %w", err)
	}

	// Ensure VolumeContext is initialised after proto deserialization; an empty
	// map is not encoded by protobuf and comes back as nil on the receiver side,
	// so a nil check is necessary before writing to it.
	if csiReq.VolumeContext == nil {
		csiReq.VolumeContext = make(map[string]string)
	}

	// Populate pod UID from downward-API env var when omitted by the caller.
	if csiReq.VolumeContext["csi.storage.k8s.io/pod.uid"] == "" {
		csiReq.VolumeContext["csi.storage.k8s.io/pod.uid"] = os.Getenv("POD_UID")
	}

	if err := validateGeneralParams(csiReq); err != nil {
		cmd.Help() // #nosec G104 -- help output error is non-actionable
		return err
	}

	originDirectory := csiReq.TargetPath
	originDirectoryMd5 := getMD5String(csiReq.TargetPath)
	log.Printf("Origin directory: %s, md5: %s", logs.SanitizeValue(originDirectory), originDirectoryMd5)

	if err := runValidateMountPath(originDirectory); err != nil {
		return fmt.Errorf("mount path validation failed: %w", err)
	}

	mountRootPath, err := mountFinderFn(mountName, debugMode)
	if err != nil {
		return fmt.Errorf("failed to find valid mount path for %q: %w", mountName, err)
	}

	provider, ok := storageLookupFn(driver)
	if !ok {
		log.Printf("Supported drivers: %v", storage.Drivers())
		cmd.Help() // #nosec G104 -- help output error is non-actionable
		return fmt.Errorf("unsupported storage driver: %s", driver)
	}

	if err = provider.Validate(csiReq); err != nil {
		cmd.Help() // #nosec G104 -- help output error is non-actionable
		return err
	}

	toMountTargetPath := path.Join(mountRootPath, provider.SubDir(), originDirectoryMd5)
	log.Printf("Real mount target path: %s", logs.SanitizeValue(toMountTargetPath))
	csiReq.TargetPath = toMountTargetPath

	if err = provider.Mount(context.Background(), csiReq, debugMode); err != nil {
		return fmt.Errorf("mount failed for driver %s: %w", driver, err)
	}

	if err := createSymlinkFn(toMountTargetPath, originDirectory); err != nil {
		return fmt.Errorf("failed to create symlink %s -> %s: %w", originDirectory, toMountTargetPath, err)
	}
	return nil
}

func mountRun(cmd *cobra.Command, args []string) {
	startTime := time.Now()
	log.Printf("Received mount request: driver=%s mountName=%s", driver, mountName)
	if err := runMount(cmd); err != nil {
		log.Printf("Mount failed (costMs=%d): %v", time.Since(startTime).Milliseconds(), logs.SanitizeValue(err.Error()))
		os.Exit(commandExitCode(err))
	}
	log.Printf("Mount succeeded (costMs=%d)", time.Since(startTime).Milliseconds())
}

func commandExitCode(err error) int {
	if errors.Is(err, mountpath.ErrUnsafeMountPath) {
		return mountpath.UnsafePathExitCode
	}
	return 1
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of sandbox-storage",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("sandbox-storage %s\n", version)
	},
}

var unmountCmd = &cobra.Command{
	Use:   "unmount",
	Short: "Unmount storage from specified path",
	Run:   unmountRun,
}

func unmountRun(cmd *cobra.Command, args []string) {
	err := validateUnmountParams() // unmount uses the same parameter validation
	if err != nil {
		log.Printf("Error: %v\n", err)
		cmd.Help() // #nosec G104 -- help output error is non-actionable
		return
	}
	// TODO
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// add sub command line
	rootCmd.AddCommand(mountCmd)             // register mount subcommand
	rootCmd.AddCommand(unmountCmd)           // register unmount subcommand
	rootCmd.AddCommand(symlinkCmd)           // register symlink subcommand
	rootCmd.AddCommand(validateMountPathCmd) // register mount-path validation subcommand
	rootCmd.AddCommand(versionCmd)           // register version subcommand

	// start to execute command line
	if err := rootCmd.Execute(); err != nil {
		log.Printf("Error: %v", err)
		os.Exit(commandExitCode(err))
	}
}

func validateGeneralParams(csiReq *csi.NodePublishVolumeRequest) error {
	if strings.TrimSpace(csiReq.VolumeContext["csi.storage.k8s.io/pod.uid"]) == "" {
		return fmt.Errorf("Pod UID is required. Use csi.storage.k8s.io/pod.uid setting")
	}
	return nil
}

func validateUnmountParams() error {
	return nil
}

func getMD5String(s string) string {
	h := md5.New()     // #nosec G401 -- non-security short hash
	h.Write([]byte(s)) // #nosec G104 -- hash.Write never returns error
	return fmt.Sprintf("%x", h.Sum(nil))
}
