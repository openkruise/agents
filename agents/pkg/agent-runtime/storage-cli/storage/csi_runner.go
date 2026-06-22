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

package storage

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"path"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// nodePublishVolumeTimeout is the upper bound for a single CSI mount RPC.
const nodePublishVolumeTimeout = 30 * time.Second

// newClientFn is the indirection used by RunNodePublishVolume to obtain a
// CSI NodeClient + a Closer for the underlying connection. It is a package
// variable so tests can substitute a fake client without binding to a real
// unix socket. Production code MUST NOT reassign it.
var newClientFn = func(socketPath string) (csi.NodeClient, io.Closer, error) {
	return newCSIClient(socketPath)
}

// RunNodePublishVolume dials the CSI plugin socket for the given driver and
// issues a NodePublishVolume RPC. It is the default mount implementation
// shared by all Provider implementations that follow the kubelet CSI socket
// layout (CsiSocketDir/<driver>/CsiSocketFile).
//
// debug controls whether the full PublishContext (which may contain credentials
// such as AK/SK or tokens) is included in the log output. Pass true only in
// non-production environments for troubleshooting.
func RunNodePublishVolume(ctx context.Context, driver string, req csi.NodePublishVolumeRequest, debug bool) error {
	socketPath := path.Join(CsiSocketDir, driver, CsiSocketFile)
	client, closer, err := newClientFn(socketPath)
	if err != nil {
		return fmt.Errorf("create CSI client for driver %q: %w", driver, err)
	}
	defer closer.Close()

	// Log non-sensitive request fields at info level.
	// PublishContext is intentionally omitted here because CSI plugins may
	// embed credentials (AK/SK, tokens) inside it. Pass debug=true to include
	// the full context for troubleshooting in non-production environments.
	log.Printf("Sending NodePublishVolume request: driver=%s volumeId=%s stagingTargetPath=%s targetPath=%s readonly=%v",
		driver, req.VolumeId, req.StagingTargetPath, req.TargetPath, req.Readonly)
	if debug {
		log.Printf("[DEBUG] NodePublishVolume publishContext: driver=%s publishContext=%v", driver, req.PublishContext)
	}

	callCtx, cancel := context.WithTimeout(ctx, nodePublishVolumeTimeout)
	defer cancel()

	start := time.Now()
	resp, err := client.NodePublishVolume(callCtx, &req, grpc.WaitForReady(true))
	if err != nil {
		return fmt.Errorf("NodePublishVolume failed for driver %q: %w", driver, err)
	}
	log.Printf("NodePublishVolume succeeded: driver=%s resp=%v costMs=%d", driver, resp, time.Since(start).Milliseconds())
	return nil
}

// newCSIClient opens a unix-socket gRPC connection to a CSI plugin.
func newCSIClient(socketPath string) (csi.NodeClient, *grpc.ClientConn, error) {
	conn, err := grpc.Dial(
		socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, addr string) (net.Conn, error) {
			return net.Dial("unix", addr)
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial socket %s: %w", socketPath, err)
	}
	return csi.NewNodeClient(conn), conn, nil
}
