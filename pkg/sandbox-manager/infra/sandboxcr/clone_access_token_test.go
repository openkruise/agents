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

package sandboxcr

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/runtime"
)

func TestPrepareSandboxFromCheckpoint_AccessToken(t *testing.T) {
	const sourceToken = "11111111-1111-4111-8111-111111111111"
	for _, tt := range []struct {
		name             string
		initRequest      string
		startupMode      v1alpha1.StartupMode
		independentToken bool
	}{
		{name: "skipped initialization", independentToken: true},
		{name: "initialized runtime", initRequest: `{"accessToken":"` + sourceToken + `"}`},
		{
			name:             "Deferred skipped initialization with retained startup data",
			initRequest:      `{"accessToken":"` + sourceToken + `","skipInit":true,"envVars":{"VALUE":"claim"},"command":["printf","claim-command"],"startupDelivered":true,"startupPodUID":"source-pod"}`,
			startupMode:      v1alpha1.StartupModeDeferred,
			independentToken: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Given a checkpoint carrying the source's optional initialization request.
			source := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name: "source", Namespace: "default",
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeAccessToken: sourceToken,
					},
				},
			}
			checkpoint := &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name: "snapshot", Namespace: "default",
					Annotations: map[string]string{v1alpha1.AnnotationInitRuntimeRequest: tt.initRequest},
				},
			}
			PropagateAnnotationsToCheckpoint(source, checkpoint)
			original := checkpoint.DeepCopy()
			sourceRequest, err := runtime.GetInitRuntimeRequest(checkpoint)
			require.NoError(t, err)
			template := &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot-template", Namespace: "default"},
				Spec: v1alpha1.SandboxTemplateSpec{
					StartupMode: tt.startupMode,
					Template:    &corev1.PodTemplateSpec{},
				},
			}
			var previousToken string
			for _, name := range []string{"clone-a", "clone-b"} {
				// When two distinct clones are prepared from the same checkpoint.
				clone, initRequest, err := prepareSandboxFromCheckpoint(t.Context(), infra.CloneSandboxOptions{
					Name: name, User: "owner", CheckPointID: checkpoint.Name,
				}, template, checkpoint, nil)
				require.NoError(t, err)
				require.Equal(t, name, clone.Name)
				require.Equal(t, tt.startupMode, clone.Spec.StartupMode)
				token := utils.GetAccessToken(clone)
				_, err = uuid.Parse(token)
				require.NoError(t, err)

				// Then only skipped-init clones get independent gateway credentials.
				if tt.independentToken {
					require.True(t, token != sourceToken, "clone must not reuse the source credential")
					require.True(t, token != previousToken, "clones must have distinct credentials")
				} else {
					require.Equal(t, sourceToken, token)
				}
				if tt.initRequest == "" {
					require.Nil(t, initRequest)
					require.NotContains(t, clone.Annotations, v1alpha1.AnnotationInitRuntimeRequest)
				} else {
					require.NotNil(t, initRequest)
					require.Equal(t, token, initRequest.AccessToken)
					stored, err := runtime.GetInitRuntimeRequest(clone.Sandbox)
					require.NoError(t, err)
					require.Equal(t, initRequest, stored, "retained request must match the clone's runtime options")
					require.Equal(t, sourceRequest.EnvVars, stored.EnvVars)
					require.Equal(t, sourceRequest.Command, stored.Command)
					require.Equal(t, sourceRequest.SkipInit, stored.SkipInit)
					if tt.startupMode == v1alpha1.StartupModeDeferred {
						require.True(t, stored.SkipInit, "retained startup data must not trigger /init")
						require.False(t, stored.StartupDelivered)
						require.Empty(t, stored.StartupPodUID)
					} else {
						require.Equal(t, tt.initRequest, clone.Annotations[v1alpha1.AnnotationInitRuntimeRequest])
					}
				}
				previousToken = token
			}
			require.Equal(t, original, checkpoint, "clone preparation must not modify its checkpoint")
		})
	}
}
