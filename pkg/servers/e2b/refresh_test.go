package e2b

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestRefreshSandboxInvalidDuration(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	_, apiError := controller.RefreshSandbox(NewRequest(
		t,
		nil,
		models.RefreshSandboxRequest{
			Duration: 3601,
		},
		map[string]string{
			"sandboxID": "dummy",
		},
		user,
	))

	assert.NotNil(t, apiError)
	if apiError != nil {
		assert.Equal(t, 400, apiError.Code)
	}
}

func TestRefreshSandboxNotFound(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	_, apiError := controller.RefreshSandbox(NewRequest(
		t,
		nil,
		models.RefreshSandboxRequest{
			Duration: 300,
		},
		map[string]string{
			"sandboxID": "does-not-exist",
		},
		user,
	))

	assert.NotNil(t, apiError)
}

func TestRefreshSandboxUpdatesTimeout(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	templateName := "test-refresh-timeout"

	cleanup := CreateSandboxPool(t, controller, templateName, 1)
	defer cleanup()

	createResp, err := controller.CreateSandbox(NewRequest(
		t,
		nil,
		models.NewSandboxRequest{
			TemplateID: templateName,
			Timeout:    600,
			Metadata: map[string]string{
				models.ExtensionKeySkipInitRuntime: v1alpha1.True,
			},
		},
		nil,
		user,
	))
	require.Nil(t, err)

	beforeRefresh := time.Now()

	_, apiError := controller.RefreshSandbox(NewRequest(
		t,
		nil,
		models.RefreshSandboxRequest{
			Duration: 900,
		},
		map[string]string{
			"sandboxID": createResp.Body.SandboxID,
		},
		user,
	))
	require.Nil(t, apiError)

	describeResp, err := controller.DescribeSandbox(NewRequest(
		t,
		nil,
		nil,
		map[string]string{
			"sandboxID": createResp.Body.SandboxID,
		},
		user,
	))
	require.Nil(t, err)

	AssertEndAt(
		t,
		beforeRefresh.Add(900*time.Second),
		describeResp.Body.EndAt,
	)

	sbx := GetSandbox(t, createResp.Body.SandboxID, client)

	require.NotNil(t, sbx.Spec.ShutdownTime)

	assert.WithinDuration(
		t,
		beforeRefresh.Add(900*time.Second),
		sbx.Spec.ShutdownTime.Time,
		5*time.Second,
	)
}

func TestRefreshSandboxNeverTimeout(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	templateName := "test-refresh-never-timeout"

	cleanup := CreateSandboxPool(t, controller, templateName, 1)
	defer cleanup()

	createResp, err := controller.CreateSandbox(NewRequest(
		t,
		nil,
		models.NewSandboxRequest{
			TemplateID: templateName,
			Timeout:    300,
			Metadata: map[string]string{
				models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				models.ExtensionKeyNeverTimeout:    v1alpha1.True,
			},
		},
		nil,
		user,
	))
	require.Nil(t, err)

	_, apiError := controller.RefreshSandbox(NewRequest(
		t,
		nil,
		models.RefreshSandboxRequest{
			Duration: 600,
		},
		map[string]string{
			"sandboxID": createResp.Body.SandboxID,
		},
		user,
	))
	require.Nil(t, apiError)

	sbx := GetSandbox(t, createResp.Body.SandboxID, client)

	assert.Nil(t, sbx.Spec.ShutdownTime)
	assert.Nil(t, sbx.Spec.PauseTime)
}
