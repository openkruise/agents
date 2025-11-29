/*
Copyright 2025.

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

package e2e

import (
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

var (
	scheme    *runtime.Scheme
	k8sClient client.Client
)

var (
	LabelDescribe = "describe"
	LabelIt       = "it"
	Namespace     = "default"
)

func init() {
	scheme = runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	c, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		panic(fmt.Sprintf("Failed to create client: %v", err))
	}
	k8sClient = c
}

// +kubebuilder:scaffold:e2e-webhooks-checks

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purpose of being used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting sandbox-operator integration test suite\n")
	customReporterConfig := types.NewDefaultReporterConfig()
	customSuiteConfig := types.NewDefaultSuiteConfig()

	//customSuiteConfig.FocusStrings = []string{"HardwareFaultHelper - Enabled"}
	customReporterConfig.Verbose = true

	RunSpecs(t, "e2e suite", customSuiteConfig, customReporterConfig)
}
