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

package runnable

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// GRPCServer converts the given gRPC server into a runnable.
// The server name is used for logging purposes.
func GRPCServer(name string, srv *grpc.Server, port int) manager.Runnable {
	return manager.RunnableFunc(func(ctx context.Context) error {
		log := ctrl.Log.WithValues("name", name)
		log.Info("gRPC server starting")

		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			log.Error(err, "gRPC server failed to listen")
			return err
		}

		log.Info("gRPC server listening", "port", port)

		// Terminate the server on context cancellation. The done channel
		// guarantees the goroutine does not leak after Serve returns.
		doneCh := make(chan struct{})
		defer close(doneCh)
		go func() {
			select {
			case <-ctx.Done():
				log.Info("gRPC server shutting down")
				srv.GracefulStop()
			case <-doneCh:
			}
		}()

		if err := srv.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			log.Error(err, "gRPC server failed")
			return err
		}
		log.Info("gRPC server terminated")
		return nil
	})
}
