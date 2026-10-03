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

// csi-sidecar-customfuse runs the CSI node server for the generic FUSE
// driver inside the sandbox csi-sidecar container. It listens on the
// per-driver socket that the storage CLI dials, and forwards mount requests
// to mount-proxy-server (which runs the FUSE entrypoint).
package main

import (
	"flag"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/openkruise/agents/pkg/agent-runtime/customfusesidecar"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

var (
	csiSocketPath = flag.String("csi-socket", "/var/run/csi/sockets/customfuseplugin.csi.openkruise.io/csi.sock",
		"path of the CSI node server socket that the storage CLI dials")
	proxySocketPath = flag.String("proxy-socket", "/var/run/csi/mounter.sock",
		"path of the mount-proxy-server socket")
)

func main() {
	klog.InitFlags(flag.CommandLine)
	flag.Parse()

	if err := run(); err != nil {
		klog.ErrorS(err, "csi-sidecar-customfuse exited with error")
		os.Exit(1)
	}
}

func run() error {
	if err := os.MkdirAll(filepath.Dir(*csiSocketPath), 0o750); err != nil {
		return err
	}
	// A stale socket from a previous run would make Listen fail with
	// EADDRINUSE. The mount namespace is fresh per sandbox, so removing any
	// pre-existing file is safe.
	if err := os.Remove(*csiSocketPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	lis, err := net.Listen("unix", *csiSocketPath)
	if err != nil {
		return err
	}
	// Remove the socket file on exit; the startup Remove above stays as
	// the fallback for an unclean previous exit. Registered BEFORE
	// lis.Close so that, under defer's LIFO order, the listener is
	// closed first and the file removed second.
	defer func() {
		if err := os.Remove(*csiSocketPath); err != nil && !os.IsNotExist(err) {
			klog.ErrorS(err, "failed to remove socket", "socket", *csiSocketPath)
		}
	}()
	defer lis.Close()
	// The storage CLI dials this socket as root on the host (same UID 0),
	// so owner-only permissions do not break it, and they keep sandbox
	// processes that can reach the file from talking to the node server
	// directly — defense in depth on top of request re-validation.
	if err := os.Chmod(*csiSocketPath, 0o600); err != nil {
		return err
	}

	// Register the signal handler before starting to serve so a SIGTERM
	// arriving in the startup window takes the graceful path instead of
	// the default action.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	srv := grpc.NewServer()
	csi.RegisterNodeServer(srv, customfusesidecar.NewNodeServer(*proxySocketPath))
	klog.InfoS("csi-sidecar-customfuse listening", "socket", *csiSocketPath, "proxy", *proxySocketPath)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(lis)
	}()

	select {
	case sig := <-sigCh:
		klog.InfoS("received signal, draining gRPC server", "signal", sig.String())
	case err := <-serveErr:
		// Serve failed before shutdown; report through the normal error
		// path so deferred cleanup runs.
		return err
	}

	// GracefulStop waits for in-flight RPCs to finish; bound the wait so
	// a stuck RPC cannot stall container shutdown. The shutdown timers
	// nest: this 8s bound sits below start.sh's 10-second SIGKILL
	// escalation, which sits below the pod's terminationGracePeriodSeconds
	// (the SandboxSet template must set it to >= 30s — a shorter window
	// would let kubelet SIGKILL cut the flush short; see the pv.yaml
	// prerequisites). All timers run from the same TERM, so this process
	// normally exits on its own terms first.
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
		// GracefulStop is done; Serve returns right after it. Reading
		// the error keeps every exit path uniform and waits until the
		// listener is fully released.
		if err := <-serveErr; err != nil {
			klog.ErrorS(err, "gRPC server returned error after graceful stop")
		}
	case err := <-serveErr:
		// Serve returned unexpectedly while draining: stop and exit
		// instead of waiting out the grace window.
		if err != nil {
			klog.ErrorS(err, "gRPC server failed while draining")
		}
		srv.Stop()
	case sig := <-sigCh:
		// A second signal while draining: stop immediately instead of
		// waiting out the full grace window.
		klog.InfoS("second signal received, forcing stop", "signal", sig.String())
		srv.Stop()
		<-serveErr
	case <-time.After(8 * time.Second):
		klog.InfoS("GracefulStop timed out, forcing stop")
		srv.Stop()
		// Stop makes Serve return; wait for it so the listener is fully
		// released before the process exits.
		<-serveErr
	}
	return nil
}
