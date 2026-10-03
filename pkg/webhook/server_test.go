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

package webhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

type serverTestManager struct {
	manager.Manager
	fakeClient client.Client
	fakeScheme *runtime.Scheme
}

func (f *serverTestManager) GetClient() client.Client   { return f.fakeClient }
func (f *serverTestManager) GetScheme() *runtime.Scheme { return f.fakeScheme }

func TestHandlerGettersIncludeSandbox(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	mgr := &serverTestManager{
		fakeClient: fake.NewClientBuilder().WithScheme(scheme).Build(),
		fakeScheme: scheme,
	}

	paths := map[string]bool{}
	for _, getter := range HandlerGetters {
		handler := getter(mgr)
		require.NotNil(t, handler)
		paths[handler.Path()] = true
	}
	assert.Contains(t, paths, "/default-sandbox")
	assert.Contains(t, paths, "/validate-sandbox")
}
