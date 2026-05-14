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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

// TestSandbox_Resume_InitRuntimeUsesPostCtx locks down the contract that when
// the original ctx deadline is consumed by the resume wait, runtime.InitRuntime
// must run on the fresh postCtx (built at sandbox.go:299-305) — same as the
// other three post-wait operations: InplaceRefresh, resolveCSIMountConfigs,
// and ProcessCSIMounts.
//
// Before the fix at sandbox.go:318, InitRuntime received the expired ctx and
// the /init request to agent-runtime failed before reaching the sidecar. With
// the fix it uses postCtx, the request lands on the sidecar, and Resume()
// returns nil with the runtime actually re-initialized.
func TestSandbox_Resume_InitRuntimeUsesPostCtx(t *testing.T) {
	utils.InitLogOutput()

	var initReceived atomic.Bool
	envdServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/init") {
			initReceived.Store(true)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer envdServer.Close()

	initOptsJSON, err := json.Marshal(config.InitRuntimeOptions{})
	require.NoError(t, err)

	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox-postctx",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeURL:         envdServer.URL,
				v1alpha1.AnnotationInitRuntimeRequest: string(initOptsJSON),
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxPaused,
			PodInfo: v1alpha1.PodInfo{
				PodIP: "10.0.0.1",
			},
		},
		Spec: v1alpha1.SandboxSpec{
			Paused: true,
		},
	}
	sandbox.Status.Conditions = append(sandbox.Status.Conditions, metav1.Condition{
		Type:   string(v1alpha1.SandboxConditionPaused),
		Status: metav1.ConditionTrue,
	})
	state, reason := sandboxutils.GetSandboxState(sandbox)
	assert.Equal(t, v1alpha1.SandboxStatePaused, state, reason)

	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())
	CreateSandboxWithStatus(t, fc, sandbox)
	time.Sleep(10 * time.Millisecond)

	s := AsSandbox(sandbox, cache)
	cache.GetMockManager().AddWaitReconcileKey(sandbox)

	// Mark sandbox Running at 190ms — just before the parent ctx 200ms deadline.
	// Forces the wait to satisfy via the double-check path right at the boundary,
	// leaving ctx.Err() != nil so postCtx is built fresh.
	modified := s.Sandbox.DeepCopy()
	mergeFrom := ctrl.MergeFrom(s.Sandbox)
	time.AfterFunc(190*time.Millisecond, func() {
		modified.Status.Phase = v1alpha1.SandboxRunning
		modified.Status.Conditions = []metav1.Condition{
			{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Resume"},
		}
		_ = fc.Status().Patch(context.Background(), modified, mergeFrom)
	})

	resumeCtx, resumeCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer resumeCancel()

	require.NoError(t, s.Resume(resumeCtx))
	require.True(t, initReceived.Load(),
		"agent-runtime /init request must reach the sidecar; if it did not, line 318 is using the expired ctx instead of postCtx")
}
