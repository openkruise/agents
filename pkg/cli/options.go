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

package cli

import (
	"fmt"
	"os"

	"github.com/spf13/pflag"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	clientset "github.com/openkruise/agents/client/clientset/versioned"
	apiv1alpha1 "github.com/openkruise/agents/client/clientset/versioned/typed/api/v1alpha1"
	kruiseversioned "github.com/openkruise/kruise-api/client/clientset/versioned"
)

var inClusterConfigFn = rest.InClusterConfig

const defaultNamespace = "default"

// GlobalOptions holds common flags shared across all CLI commands.
type GlobalOptions struct {
	KubeConfig string
	Namespace  string
	Context    string
}

// NewGlobalOptions returns a GlobalOptions with default values.
func NewGlobalOptions() *GlobalOptions {
	return &GlobalOptions{
		Namespace: defaultNamespace,
	}
}

// AddFlags registers common flags on the provided FlagSet.
func (o *GlobalOptions) AddFlags(flags *pflag.FlagSet) {
	flags.StringVar(&o.KubeConfig, "kubeconfig", o.KubeConfig, "Path to the kubeconfig file (defaults to ~/.kube/config)")
	flags.StringVarP(&o.Namespace, "namespace", "n", o.Namespace, "Namespace scope for this request")
	flags.StringVar(&o.Context, "context", o.Context, "Kubeconfig context to use (overrides current-context)")
}

// RESTConfig builds a rest.Config from the current flags.
// When running inside a Pod (no explicit kubeconfig), it uses the mounted ServiceAccount token.
// When running locally, it falls back to the kubeconfig file.
func (o *GlobalOptions) RESTConfig() (*rest.Config, error) {
	if o.KubeConfig == "" && o.Context == "" {
		if cfg, err := inClusterConfigFn(); err == nil {
			return cfg, nil
		} else {
			fmt.Fprintf(os.Stderr, "Warning: in-cluster config failed (%v), falling back to kubeconfig\n", err)
		}
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.KubeConfig != "" {
		loadingRules.ExplicitPath = o.KubeConfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if o.Context != "" {
		overrides.CurrentContext = o.Context
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}
	return config, nil
}

// AgentsClient builds an ApiV1alpha1Interface from the current flags.
func (o *GlobalOptions) AgentsClient() (apiv1alpha1.ApiV1alpha1Interface, error) {
	config, err := o.RESTConfig()
	if err != nil {
		return nil, err
	}

	cs, err := clientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create agents clientset: %w", err)
	}
	return cs.ApiV1alpha1(), nil
}

// KruiseClient builds a kruise-api clientset from the current flags.
func (o *GlobalOptions) KruiseClient() (kruiseversioned.Interface, error) {
	config, err := o.RESTConfig()
	if err != nil {
		return nil, err
	}

	cs, err := kruiseversioned.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kruise clientset: %w", err)
	}
	return cs, nil
}

// DynamicClient builds a dynamic.Interface from the current flags.
func (o *GlobalOptions) DynamicClient() (dynamic.Interface, error) {
	config, err := o.RESTConfig()
	if err != nil {
		return nil, err
	}

	dc, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}
	return dc, nil
}

// KubeClient builds a kubernetes.Interface from the current flags.
func (o *GlobalOptions) KubeClient() (kubernetes.Interface, error) {
	config, err := o.RESTConfig()
	if err != nil {
		return nil, err
	}

	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube clientset: %w", err)
	}
	return cs, nil
}
