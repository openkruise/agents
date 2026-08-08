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

package poolautoscaler

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

// fakeManager embeds manager.Manager (nil) and overrides only the methods
// that GetHandlerGetters' handler getter calls: GetClient and GetScheme.
type fakeManager struct {
	manager.Manager
	fakeClient client.Client
	fakeScheme *runtime.Scheme
}

func (f *fakeManager) GetClient() client.Client   { return f.fakeClient }
func (f *fakeManager) GetScheme() *runtime.Scheme { return f.fakeScheme }

func TestGetHandlerGetters(t *testing.T) {
	getters := GetHandlerGetters()

	t.Run("returns 1 handler getter", func(t *testing.T) {
		assert.Len(t, getters, 1)
	})

	t.Run("handler getter returns non-nil Handler with correct path", func(t *testing.T) {
		require.NotEmpty(t, getters)

		scheme := runtime.NewScheme()
		_ = clientgoscheme.AddToScheme(scheme)
		_ = agentsv1alpha1.AddToScheme(scheme)
		fc := fake.NewClientBuilder().WithScheme(scheme).Build()

		mgr := &fakeManager{
			fakeClient: fc,
			fakeScheme: scheme,
		}

		handler := getters[0](mgr)
		require.NotNil(t, handler)
		assert.Equal(t, "/validate-poolautoscaler", handler.Path())
		assert.True(t, handler.Enabled())
	})
}
