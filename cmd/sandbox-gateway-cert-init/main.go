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
	"fmt"
	"log"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/openkruise/agents/pkg/sandbox-gateway/runtimecredentials"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatalf("initialize runtime mTLS credentials: %v", err)
	}
}

func run(ctx context.Context) error {
	opts, err := runtimecredentials.OptionsFromEnvironment()
	if err != nil {
		return err
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("get in-cluster configuration: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	return runtimecredentials.Load(ctx, client, opts)
}
