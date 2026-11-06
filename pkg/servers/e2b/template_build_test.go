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

package e2b

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandbox-manager/templatecr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

func intPtr(value int) *int {
	return &value
}

// templateBuildSetupVariant adjusts the controller the setup builds: extra
// interceptors wrap the TemplateStore client, and withoutCache leaves the
// infra cache unset to exercise the guards that treat it as optional.
type templateBuildSetupVariant struct {
	extraInterceptors []interceptor.Funcs
	withoutCache      bool
}

// setupTemplateBuildController builds a controller whose manager carries a
// TemplateStore over a fake client that assigns the uid and creation time the
// apiserver provides in production; without them every reservation would be
// rejected for lacking a Build identity. The optional wrapper replaces the
// SandboxSet cache the reference check reads.
func setupTemplateBuildController(t *testing.T, wrapSandboxSets func(infracache.Provider) infracache.Provider, variants ...templateBuildSetupVariant) (*Controller, ctrlclient.Client) {
	t.Helper()
	var variant templateBuildSetupVariant
	if len(variants) > 0 {
		variant = variants[0]
	}
	baseCache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	sandboxSets := infracache.Provider(baseCache)
	if wrapSandboxSets != nil {
		sandboxSets = wrapSandboxSets(baseCache)
	}
	buildClient := interceptor.NewClient(fc.(ctrlclient.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
			if template, ok := obj.(*v1alpha1.SandboxTemplate); ok && template.UID == "" {
				template.UID = types.UID(uuid.NewString())
				template.CreationTimestamp = metav1.Now()
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	for _, funcs := range variant.extraInterceptors {
		buildClient = interceptor.NewClient(buildClient, funcs)
	}
	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    "sandbox-system",
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	manager, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			builder := sandboxcr.NewInfraBuilder(opts).
				WithAPIReader(baseCache.GetAPIReader()).
				WithRouteReader(proxy.NewServer(opts))
			if !variant.withoutCache {
				builder = builder.WithCache(sandboxSets)
			}
			return builder, nil
		}).
		WithTemplateStore(templatecr.NewStore(buildClient, func() ctrlclient.Reader { return buildClient })).
		Build()
	require.NoError(t, err)
	return &Controller{domain: "example.com", cache: baseCache, manager: manager}, fc
}

func templateBuildUser() *models.CreatedTeamAPIKey {
	return &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  "team-a-key",
		Name: "team-a-user",
		Team: &models.Team{Name: "team-a"},
	}
}

func reserveBuild(t *testing.T, controller *Controller, name string) *models.TemplateRequestResponseV3 {
	t.Helper()
	resp, apiErr := controller.ReserveTemplateBuild(NewRequest(t, nil, models.TemplateBuildRequestV3{Name: name}, nil, templateBuildUser()))
	require.Nil(t, apiErr)
	require.Equal(t, http.StatusAccepted, resp.Code)
	return resp.Body
}

func submitBuildRequest(t *testing.T, templateID, buildID string, body any) *http.Request {
	t.Helper()
	return NewRequest(t, nil, body, map[string]string{"templateID": templateID, "buildID": buildID}, templateBuildUser())
}

// templateBuildFixture builds a submitted TemplateBuild whose workload
// projects cleanly: one business container with the 1-cpu, 1024MiB envelope.
func templateBuildFixture(name, buildID string, phase v1alpha1.SandboxTemplateBuildPhase) *v1alpha1.SandboxTemplate {
	envelope := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1"),
		corev1.ResourceMemory: resource.MustParse("1024Mi"),
	}
	condition := metav1.Condition{
		Type:               v1alpha1.SandboxTemplateBuildConditionReady,
		LastTransitionTime: metav1.Now(),
	}
	switch phase {
	case v1alpha1.SandboxTemplatePhaseFailed:
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "ValidationFailed", "spec.template.spec.containers must not be empty"
	case v1alpha1.SandboxTemplatePhaseReady:
		condition.Status, condition.Reason = metav1.ConditionTrue, "Ready"
	default:
		condition.Status, condition.Reason = metav1.ConditionUnknown, "BuildReserved"
	}
	return &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			Name:      name,
			UID:       types.UID(buildID),
			Annotations: map[string]string{
				v1alpha1.AnnotationTemplateBuildState: v1alpha1.TemplateBuildStateSubmitted,
			},
			CreationTimestamp: metav1.Now(),
		},
		Spec: v1alpha1.SandboxTemplateSpec{
			Backend: v1alpha1.SandboxTemplateBackendKubernetes,
			Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  "runtime",
				Image: "python:3.12-slim",
				Resources: corev1.ResourceRequirements{
					Requests: envelope.DeepCopy(),
					Limits:   envelope.DeepCopy(),
				},
			}}}},
		},
		Status: v1alpha1.SandboxTemplateStatus{
			Phase:      phase,
			Conditions: []metav1.Condition{condition},
		},
	}
}

func TestTemplateBuildErrorMapping(t *testing.T) {
	for _, tt := range []struct {
		name    string
		code    managererrors.ErrorCode
		wantAPI int
	}{
		{name: "bad request", code: managererrors.ErrorBadRequest, wantAPI: http.StatusBadRequest},
		{name: "not found", code: managererrors.ErrorNotFound, wantAPI: http.StatusNotFound},
		{name: "conflict", code: managererrors.ErrorConflict, wantAPI: http.StatusConflict},
		{name: "unconfigured backend", code: managererrors.ErrorNotImplemented, wantAPI: http.StatusNotImplemented},
		{name: "unavailable storage", code: managererrors.ErrorUnavailable, wantAPI: http.StatusServiceUnavailable},
		{name: "internal", code: managererrors.ErrorInternal, wantAPI: http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := templateBuildError(managererrors.NewError(tt.code, "build %q failed", "b"))
			assert.Equal(t, tt.wantAPI, apiErr.Code)
		})
	}
}

func TestReserveTemplateBuild(t *testing.T) {
	controller, fc := setupTemplateBuildController(t, nil)

	reserved := reserveBuild(t, controller, "python-agent")
	assert.Equal(t, "python-agent", reserved.TemplateID)
	assert.NotEmpty(t, reserved.BuildID)
	assert.False(t, reserved.Public)
	assert.Equal(t, []string{"python-agent"}, reserved.Names)
	assert.Equal(t, []string{"default"}, reserved.Tags)
	assert.Empty(t, reserved.Aliases)

	stored := &v1alpha1.SandboxTemplate{}
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-a", Name: "python-agent"}, stored))
	assert.Equal(t, v1alpha1.TemplateBuildStateReserved, stored.Annotations[v1alpha1.AnnotationTemplateBuildState])
	assert.Equal(t, reserved.BuildID, string(stored.UID))

	// A same-parameters retry replays the reservation: the SDK auto-retry
	// after a lost response keeps working and never bricks the name.
	retried, apiErr := controller.ReserveTemplateBuild(NewRequest(t, nil, models.TemplateBuildRequestV3{Name: "python-agent"}, nil, templateBuildUser()))
	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusAccepted, retried.Code)
	assert.Equal(t, reserved.BuildID, retried.Body.BuildID)

	// Different reservation parameters still conflict.
	cpuCount := 4
	_, apiErr = controller.ReserveTemplateBuild(NewRequest(t, nil, models.TemplateBuildRequestV3{Name: "python-agent", CPUCount: &cpuCount}, nil, templateBuildUser()))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.Code)
}

func TestReserveTemplateBuildAcceptsNamingAndResourceParameters(t *testing.T) {
	controller, fc := setupTemplateBuildController(t, nil)
	cpuCount, memoryMB := 2, 2048

	resp, apiErr := controller.ReserveTemplateBuild(NewRequest(t, nil, models.TemplateBuildRequestV3{
		Name:     "derived:default",
		CPUCount: &cpuCount,
		MemoryMB: &memoryMB,
	}, nil, templateBuildUser()))
	require.Nil(t, apiErr)
	assert.Equal(t, "derived", resp.Body.TemplateID)
	assert.Equal(t, []string{"derived"}, resp.Body.Names)

	stored := &v1alpha1.SandboxTemplate{}
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-a", Name: "derived"}, stored))
	requests := stored.Spec.Template.Spec.Containers[0].Resources.Requests
	assert.True(t, requests.Cpu().Equal(resource.MustParse("2")))
	assert.True(t, requests.Memory().Equal(resource.MustParse("2048Mi")))
}

func TestReserveTemplateBuildRejectsInput(t *testing.T) {
	valid := models.TemplateBuildRequestV3{Name: "template"}
	tests := []struct {
		name   string
		body   any
		header map[string]string
	}{
		{name: "unknown tag in name", body: models.TemplateBuildRequestV3{Name: "template:beta"}},
		{name: "custom tags", body: models.TemplateBuildRequestV3{Name: "template", Tags: []string{"beta"}}},
		{name: "alias", body: models.TemplateBuildRequestV3{Name: "template", Alias: "alias"}},
		{name: "teamID", body: models.TemplateBuildRequestV3{Name: "template", TeamID: "team"}},
		{name: "minFreeDiskMb", body: models.TemplateBuildRequestV3{Name: "template", MinFreeDiskMB: intPtr(1024)}},
		{name: "zero cpu", body: models.TemplateBuildRequestV3{Name: "template", CPUCount: intPtr(0)}},
		{name: "negative memory", body: models.TemplateBuildRequestV3{Name: "template", MemoryMB: intPtr(-1)}},
		{name: "unknown body field", body: map[string]any{"name": "template", "beta": "1"}},
		{name: "auto-warm header", body: valid, header: map[string]string{"x-e2b-kruise-auto-warm": "true"}},
		{name: "warmpool-size header", body: valid, header: map[string]string{"x-e2b-kruise-warmpool-size": "2"}},
		{name: "unknown kruise header", body: valid, header: map[string]string{"x-e2b-kruise-beta": "1"}},
		{name: "unknown infra value", body: valid, header: map[string]string{"x-e2b-kruise-infra": "beta"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, _ := setupTemplateBuildController(t, nil)
			req := NewRequest(t, nil, tt.body, nil, templateBuildUser())
			for key, value := range tt.header {
				req.Header.Set(key, value)
			}
			_, apiErr := controller.ReserveTemplateBuild(req)
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		})
	}
}

func TestReserveTemplateBuildBackendHeader(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		wantErr int
	}{
		{name: "k8s accepted", values: []string{"k8s"}},
		{name: "absent accepted as the default", values: nil},
		{name: "unknown backend value", values: []string{"beta"}, wantErr: http.StatusBadRequest},
		{name: "repeated values", values: []string{"k8s", "k8s"}, wantErr: http.StatusBadRequest},
		{name: "empty value is invalid", values: []string{""}, wantErr: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, _ := setupTemplateBuildController(t, nil)
			req := NewRequest(t, nil, models.TemplateBuildRequestV3{Name: "template"}, nil, templateBuildUser())
			for _, value := range tt.values {
				req.Header.Add("x-e2b-kruise-infra", value)
			}
			_, apiErr := controller.ReserveTemplateBuild(req)
			if tt.wantErr == 0 {
				require.Nil(t, apiErr)
				return
			}
			require.NotNil(t, apiErr)
			assert.Equal(t, tt.wantErr, apiErr.Code)
		})
	}
}

func TestSubmitTemplateBuild(t *testing.T) {
	controller, fc := setupTemplateBuildController(t, nil)
	reserved := reserveBuild(t, controller, "template")

	resp, apiErr := controller.SubmitTemplateBuild(submitBuildRequest(t, "template", reserved.BuildID, models.TemplateBuildStartV2{
		FromImage: strPtr("python:3.12-slim"),
		Steps:     []models.TemplateStep{{Type: "RUN", Args: []string{"python -m pip install numpy", "root"}}},
	}))
	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusAccepted, resp.Code)

	stored := &v1alpha1.SandboxTemplate{}
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-a", Name: "template"}, stored))
	assert.Equal(t, v1alpha1.TemplateBuildStateSubmitted, stored.Annotations[v1alpha1.AnnotationTemplateBuildState])
	require.NotNil(t, stored.Spec.Template)
	assert.Equal(t, "python:3.12-slim", stored.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, []v1alpha1.RuntimeConfig{{Name: v1alpha1.RuntimeConfigForInjectAgentRuntime}}, stored.Spec.Runtimes)
	assert.Equal(t, []string{v1alpha1.PersistentContentFilesystem}, stored.Spec.PersistentContents)
	require.NotNil(t, stored.Spec.BuildOptions)
	assert.Equal(t, []v1alpha1.TemplateBuildCommand{{Command: "python -m pip install numpy", User: "root"}}, stored.Spec.BuildOptions.Steps)
	// The reserved envelope is applied to the business container.
	assert.True(t, stored.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().Equal(resource.MustParse("1")))
	assert.True(t, stored.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Equal(resource.MustParse("1024Mi")))

	// A retry carrying the same Build identity replays the committed
	// submission; the input is not compared — the Build was minted by the
	// reservation and is known only to the reserving caller.
	retried, apiErr := controller.SubmitTemplateBuild(submitBuildRequest(t, "template", reserved.BuildID, models.TemplateBuildStartV2{FromImage: strPtr("python:3.13")}))
	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusAccepted, retried.Code)

	// A foreign Build identity still conflicts.
	_, apiErr = controller.SubmitTemplateBuild(submitBuildRequest(t, "template", "other-build", models.TemplateBuildStartV2{FromImage: strPtr("python:3.12-slim")}))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.Code)
}

func TestSubmitTemplateBuildDeclaresMemoryForLifecycleCommands(t *testing.T) {
	controller, fc := setupTemplateBuildController(t, nil)
	reserved := reserveBuild(t, controller, "template")

	_, apiErr := controller.SubmitTemplateBuild(submitBuildRequest(t, "template", reserved.BuildID, models.TemplateBuildStartV2{
		FromImage: strPtr("python:3.12-slim"),
		ReadyCmd:  strPtr("python -m http.server 8000"),
	}))
	require.Nil(t, apiErr)

	stored := &v1alpha1.SandboxTemplate{}
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-a", Name: "template"}, stored))
	assert.ElementsMatch(t, []string{v1alpha1.PersistentContentFilesystem, v1alpha1.PersistentContentMemory}, stored.Spec.PersistentContents)
}

func TestSubmitTemplateBuildRejectsInput(t *testing.T) {
	controller, _ := setupTemplateBuildController(t, nil)
	reserved := reserveBuild(t, controller, "template")

	// Unknown reservation: the templateID does not resolve in the namespace.
	_, apiErr := controller.SubmitTemplateBuild(submitBuildRequest(t, "missing", reserved.BuildID, models.TemplateBuildStartV2{FromImage: strPtr("image")}))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.Code)

	// Foreign build identity.
	_, apiErr = controller.SubmitTemplateBuild(submitBuildRequest(t, "template", "other-build", models.TemplateBuildStartV2{FromImage: strPtr("image")}))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.Code)

	// Protocol input the conversion layer rejects.
	_, apiErr = controller.SubmitTemplateBuild(submitBuildRequest(t, "template", reserved.BuildID, models.TemplateBuildStartV2{
		Steps: []models.TemplateStep{{Type: "COPY", Args: []string{"a", "b"}}},
	}))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)

	// Unknown body fields stay rejected on submission.
	_, apiErr = controller.SubmitTemplateBuild(submitBuildRequest(t, "template", reserved.BuildID, map[string]any{"fromImage": "image", "beta": "1"}))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)

	// Companion headers stay rejected on submission.
	req := submitBuildRequest(t, "template", reserved.BuildID, models.TemplateBuildStartV2{FromImage: strPtr("image")})
	req.Header.Set("x-e2b-kruise-auto-warm", "true")
	_, apiErr = controller.SubmitTemplateBuild(req)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)
}

func TestGetTemplateBuildStatus(t *testing.T) {
	tests := []struct {
		name       string
		phase      v1alpha1.SandboxTemplateBuildPhase
		templateID string
		buildID    string
		wantStatus string
		wantCode   int
	}{
		{name: "pending draft is waiting", phase: v1alpha1.SandboxTemplatePhasePending, wantStatus: "waiting"},
		{name: "ready build reports ready", phase: v1alpha1.SandboxTemplatePhaseReady, wantStatus: "ready"},
		{name: "building build reports building", phase: v1alpha1.SandboxTemplatePhaseBuilding, wantStatus: "building"},
		{name: "failed build reports the condition message", phase: v1alpha1.SandboxTemplatePhaseFailed, wantStatus: "error"},
		{name: "foreign build id is not found", phase: v1alpha1.SandboxTemplatePhaseReady, buildID: "other-build", wantCode: http.StatusNotFound},
		{name: "unknown template", templateID: "missing", wantCode: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, fc := setupTemplateBuildController(t, nil)
			require.NoError(t, fc.Create(t.Context(), templateBuildFixture("template", "build-uid", tt.phase)))
			templateID, buildID := tt.templateID, tt.buildID
			if templateID == "" {
				templateID = "template"
			}
			if buildID == "" {
				buildID = "build-uid"
			}
			resp, apiErr := controller.GetTemplateBuildStatus(NewRequest(t, nil, nil,
				map[string]string{"templateID": templateID, "buildID": buildID}, templateBuildUser()))
			if tt.wantCode != 0 {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.wantCode, apiErr.Code)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, http.StatusOK, resp.Code)
			assert.Equal(t, "template", resp.Body.TemplateID)
			assert.Equal(t, tt.wantStatus, resp.Body.Status)
			assert.Empty(t, resp.Body.Logs)
			assert.Empty(t, resp.Body.LogEntries)
			if tt.phase == v1alpha1.SandboxTemplatePhaseFailed {
				require.NotNil(t, resp.Body.Reason)
				assert.Contains(t, resp.Body.Reason.Message, "must not be empty")
			} else {
				assert.Nil(t, resp.Body.Reason)
			}
		})
	}
}

func TestGetTemplateBuildStatusLogQuery(t *testing.T) {
	setup := func(t *testing.T) *Controller {
		controller, fc := setupTemplateBuildController(t, nil)
		require.NoError(t, fc.Create(t.Context(), templateBuildFixture("template", "build-uid", v1alpha1.SandboxTemplatePhaseReady)))
		return controller
	}
	statusRequest := func(query map[string]string) *http.Request {
		return NewRequest(t, query, nil, map[string]string{"templateID": "template", "buildID": "build-uid"}, templateBuildUser())
	}

	for _, tt := range []struct {
		name  string
		query map[string]string
		code  int
	}{
		{name: "level filtering", query: map[string]string{"level": "info"}, code: http.StatusBadRequest},
		{name: "negative offset", query: map[string]string{"logsOffset": "-1"}, code: http.StatusBadRequest},
		{name: "invalid offset", query: map[string]string{"logsOffset": "beta"}, code: http.StatusBadRequest},
		{name: "zero limit", query: map[string]string{"limit": "0"}, code: http.StatusBadRequest},
		{name: "excessive limit", query: map[string]string{"limit": "101"}, code: http.StatusBadRequest},
		// The generated Python client sends limit=100 by default and the JS
		// SDK sends no query at all.
		{name: "client defaults", query: map[string]string{"limit": "100", "logsOffset": "0"}, code: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp, apiErr := setup(t).GetTemplateBuildStatus(statusRequest(tt.query))
			if tt.code == http.StatusOK {
				require.Nil(t, apiErr)
				assert.Equal(t, "ready", resp.Body.Status)
				return
			}
			require.NotNil(t, apiErr)
			assert.Equal(t, tt.code, apiErr.Code)
		})
	}
}

func TestGetTemplateBuildStatusRejectsCompanionHeaders(t *testing.T) {
	controller, _ := setupTemplateBuildController(t, nil)
	req := NewRequest(t, nil, nil, map[string]string{"templateID": "template", "buildID": "build-uid"}, templateBuildUser())
	req.Header.Set("x-e2b-kruise-auto-warm", "true")
	_, apiErr := controller.GetTemplateBuildStatus(req)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)
}

func TestReserveTemplateBuildRejectsOccupiedName(t *testing.T) {
	t.Run("sandboxset occupies the name", func(t *testing.T) {
		controller, fc := setupTemplateBuildController(t, nil)
		require.NoError(t, fc.Create(t.Context(), &v1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{Name: "occupied", Namespace: "team-a"},
		}))
		require.Eventually(t, func() bool {
			return controller.manager.GetInfra().HasTemplate(t.Context(), infra.HasTemplateOptions{Namespace: "team-a", Name: "occupied"})
		}, time.Second, 10*time.Millisecond)

		_, apiErr := controller.ReserveTemplateBuild(NewRequest(t, nil, models.TemplateBuildRequestV3{Name: "occupied"}, nil, templateBuildUser()))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusConflict, apiErr.Code)
	})

	t.Run("checkpoint occupies the name", func(t *testing.T) {
		controller, fc := setupTemplateBuildController(t, nil)
		checkpoint := &v1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "occupied", Namespace: "team-a"},
			Status:     v1alpha1.CheckpointStatus{CheckpointId: "occupied"},
		}
		require.NoError(t, fc.Create(t.Context(), checkpoint))
		require.NoError(t, fc.Status().Update(t.Context(), checkpoint))
		require.Eventually(t, func() bool {
			return controller.manager.GetInfra().HasCheckpoint(t.Context(), infra.HasCheckpointOptions{Namespace: "team-a", CheckpointID: "occupied"})
		}, time.Second, 10*time.Millisecond)

		_, apiErr := controller.ReserveTemplateBuild(NewRequest(t, nil, models.TemplateBuildRequestV3{Name: "occupied"}, nil, templateBuildUser()))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusConflict, apiErr.Code)
	})

	t.Run("no occupation elsewhere in the namespace", func(t *testing.T) {
		controller, fc := setupTemplateBuildController(t, nil)
		require.NoError(t, fc.Create(t.Context(), &v1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team-b"},
		}))

		resp, apiErr := controller.ReserveTemplateBuild(NewRequest(t, nil, models.TemplateBuildRequestV3{Name: "free-name"}, nil, templateBuildUser()))
		require.Nil(t, apiErr)
		assert.Equal(t, http.StatusAccepted, resp.Code)
	})
}

func TestGetTemplateAlias(t *testing.T) {
	controller, fc := setupTemplateBuildController(t, nil)
	require.NoError(t, fc.Create(t.Context(), templateBuildFixture("python-agent", "build-uid", v1alpha1.SandboxTemplatePhaseReady)))

	tests := []struct {
		name     string
		alias    string
		wantCode int
	}{
		{name: "bound name exists", alias: "python-agent", wantCode: http.StatusOK},
		{name: "default-tag suffix resolves", alias: "python-agent:default", wantCode: http.StatusOK},
		{name: "custom tag rejected", alias: "python-agent:beta", wantCode: http.StatusBadRequest},
		{name: "absent name", alias: "missing", wantCode: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, apiErr := controller.GetTemplateAlias(NewRequest(t, nil, nil,
				map[string]string{"alias": tt.alias}, templateBuildUser()))
			if tt.wantCode != http.StatusOK {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.wantCode, apiErr.Code)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, http.StatusOK, resp.Code)
			assert.Equal(t, "python-agent", resp.Body.TemplateID)
			assert.False(t, resp.Body.Public)
		})
	}
}

// A request without an authenticated user cannot resolve a namespace, so every
// build route reports the middleware invariant violation.
func TestTemplateBuildRoutesWithoutUser(t *testing.T) {
	controller, _ := setupTemplateBuildController(t, nil)
	_, reserveErr := controller.ReserveTemplateBuild(NewRequest(t, nil, models.TemplateBuildRequestV3{Name: "template"}, nil, nil))
	_, submitErr := controller.SubmitTemplateBuild(NewRequest(t, nil, models.TemplateBuildStartV2{FromImage: strPtr("image")},
		map[string]string{"templateID": "template", "buildID": "build-uid"}, nil))
	_, statusErr := controller.GetTemplateBuildStatus(NewRequest(t, nil, nil, map[string]string{"templateID": "template", "buildID": "build-uid"}, nil))
	_, aliasErr := controller.GetTemplateAlias(NewRequest(t, nil, nil, map[string]string{"alias": "template"}, nil))
	for name, apiErr := range map[string]*web.ApiError{
		"reserve": reserveErr,
		"submit":  submitErr,
		"status":  statusErr,
		"alias":   aliasErr,
	} {
		t.Run(name, func(t *testing.T) {
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
			assert.Equal(t, "User not found", apiErr.Message)
		})
	}
}

func TestGetTemplateProjectsTemplateBuildFirst(t *testing.T) {
	controller, fc := setupTemplateBuildController(t, nil)
	withDisk := templateBuildFixture("python-agent", "build-uid", v1alpha1.SandboxTemplatePhaseReady)
	withDisk.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}}
	require.NoError(t, fc.Create(t.Context(), withDisk))

	resp, apiErr := controller.GetTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "python-agent"}, templateBuildUser()))
	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusOK, resp.Code)
	assert.Equal(t, "python-agent", resp.Body.TemplateID)
	assert.False(t, resp.Body.Public)
	assert.Equal(t, []string{"python-agent"}, resp.Body.Names)
	require.Len(t, resp.Body.Builds, 1)
	assert.Equal(t, "build-uid", resp.Body.Builds[0].BuildID)
	assert.Equal(t, "ready", resp.Body.Builds[0].Status)
	assert.Equal(t, 1, resp.Body.Builds[0].CPUCount)
	assert.Equal(t, 1024, resp.Body.Builds[0].MemoryMB)
	assert.Equal(t, 1024, resp.Body.Builds[0].DiskSizeMB)

	// The reserved name is never served through any route.
	_, apiErr = controller.GetTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "tags"}, templateBuildUser()))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.Code)

	// A workload the protocol cannot represent reports 400 instead of a
	// distorted projection.
	custom := templateBuildFixture("custom", "custom-uid", v1alpha1.SandboxTemplatePhaseReady)
	custom.Spec.Template.Spec.Containers = append(custom.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar"})
	require.NoError(t, fc.Create(t.Context(), custom))
	_, apiErr = controller.GetTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "custom"}, templateBuildUser()))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)

	// Resource envelopes the protocol cannot project report 400 as well.
	for name, tweak := range map[string]func(*v1alpha1.SandboxTemplate){
		"fractional-cpu": func(tmpl *v1alpha1.SandboxTemplate) {
			c := &tmpl.Spec.Template.Spec.Containers[0]
			c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
			c.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("500m")
		},
		"unequal-memory": func(tmpl *v1alpha1.SandboxTemplate) {
			tmpl.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2048Mi")
		},
	} {
		t.Run(name, func(t *testing.T) {
			unrepresentable := templateBuildFixture(name, "custom-uid", v1alpha1.SandboxTemplatePhaseReady)
			tweak(unrepresentable)
			require.NoError(t, fc.Create(t.Context(), unrepresentable))
			_, apiErr := controller.GetTemplate(NewRequest(t, nil, nil,
				map[string]string{"templateID": name}, templateBuildUser()))
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		})
	}
}

func TestDeleteTemplateRemovesTemplateBuild(t *testing.T) {
	controller, fc := setupTemplateBuildController(t, nil)
	require.NoError(t, fc.Create(t.Context(), templateBuildFixture("python-agent", "build-uid", v1alpha1.SandboxTemplatePhaseReady)))

	resp, apiErr := controller.DeleteTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "python-agent"}, templateBuildUser()))
	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusNoContent, resp.Code)
	err := fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-a", Name: "python-agent"}, &v1alpha1.SandboxTemplate{})
	require.True(t, apierrors.IsNotFound(err))

	// A repeated delete maps the missing template to success.
	resp, apiErr = controller.DeleteTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "python-agent"}, templateBuildUser()))
	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusNoContent, resp.Code)
}

func TestDeleteTemplateRejectsReferencedName(t *testing.T) {
	spy := &listSandboxSetsSpy{result: []*v1alpha1.SandboxSet{{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "team-a"},
		Spec: v1alpha1.SandboxSetSpec{
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "python-agent"},
			},
		},
	}}}
	controller, fc := setupTemplateBuildController(t, func(provider infracache.Provider) infracache.Provider {
		spy.Provider = provider
		return spy
	})
	require.NoError(t, fc.Create(t.Context(), templateBuildFixture("python-agent", "build-uid", v1alpha1.SandboxTemplatePhaseReady)))

	_, apiErr := controller.DeleteTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "python-agent"}, templateBuildUser()))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.Code)
	// The reference check reaches the cache through the templateRef index.
	assert.True(t, spy.called)
	assert.Equal(t, "team-a", spy.namespace)
	assert.Equal(t, "python-agent", spy.templateRef)
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-a", Name: "python-agent"}, &v1alpha1.SandboxTemplate{}))

	// The reserved name never enters delete semantics.
	_, apiErr = controller.DeleteTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "tags"}, templateBuildUser()))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.Code)
}

func TestDeleteTemplateReportsListFailure(t *testing.T) {
	spy := &listSandboxSetsSpy{err: assert.AnError}
	controller, fc := setupTemplateBuildController(t, func(provider infracache.Provider) infracache.Provider {
		spy.Provider = provider
		return spy
	})
	require.NoError(t, fc.Create(t.Context(), templateBuildFixture("python-agent", "build-uid", v1alpha1.SandboxTemplatePhaseReady)))

	_, apiErr := controller.DeleteTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "python-agent"}, templateBuildUser()))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
	// The failed reference listing must not reach the deletion.
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-a", Name: "python-agent"}, &v1alpha1.SandboxTemplate{}))
}

// A storage failure on the TemplateBuild read surfaces as 503 on both the
// details and the delete route, and the failed read never reaches the deletion.
func TestTemplateRoutesReportStorageFailures(t *testing.T) {
	controller, fc := setupTemplateBuildController(t, nil, templateBuildSetupVariant{
		extraInterceptors: []interceptor.Funcs{{
			Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
				return apierrors.NewServiceUnavailable("storage unavailable")
			},
		}},
	})
	require.NoError(t, fc.Create(t.Context(), templateBuildFixture("python-agent", "build-uid", v1alpha1.SandboxTemplatePhaseReady)))

	_, apiErr := controller.GetTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "python-agent"}, templateBuildUser()))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.Code)

	_, apiErr = controller.DeleteTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "python-agent"}, templateBuildUser()))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.Code)
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-a", Name: "python-agent"}, &v1alpha1.SandboxTemplate{}))
}

// A manager whose infra carries no cache cannot check references, so the
// delete refuses instead of bypassing the reference protection.
func TestDeleteTemplateWithoutCache(t *testing.T) {
	controller, _ := setupTemplateBuildController(t, nil, templateBuildSetupVariant{withoutCache: true})

	_, apiErr := controller.DeleteTemplate(NewRequest(t, nil, nil,
		map[string]string{"templateID": "python-agent"}, templateBuildUser()))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
	assert.Equal(t, "Cache not available", apiErr.Message)
}
