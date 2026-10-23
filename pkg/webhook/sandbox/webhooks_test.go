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

package sandbox

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

type fakeManager struct {
	manager.Manager
	fakeClient client.Client
	fakeScheme *runtime.Scheme
}

func (f *fakeManager) GetClient() client.Client   { return f.fakeClient }
func (f *fakeManager) GetScheme() *runtime.Scheme { return f.fakeScheme }

func TestGetHandlerGetters(t *testing.T) {
	getters := GetHandlerGetters()
	require.Len(t, getters, 2)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &fakeManager{fakeClient: fc, fakeScheme: scheme}

	paths := map[string]bool{}
	for i, getter := range getters {
		handler := getter(mgr)
		require.NotNil(t, handler, "getter %d returned nil handler", i)
		assert.True(t, handler.Enabled())
		paths[handler.Path()] = true
	}
	assert.Contains(t, paths, "/default-sandbox")
	assert.Contains(t, paths, "/validate-sandbox")
}
