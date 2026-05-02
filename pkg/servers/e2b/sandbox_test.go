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
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestReplacer(t *testing.T) {
	url := "ws://localhost:9222/devtools/browser/12345678-1234-1234-1234-123456789012"
	url = browserWebSocketReplacer.ReplaceAllString(url, "ws://hello-world")
	if url != "ws://hello-world/devtools/browser/12345678-1234-1234-1234-123456789012" {
		t.Errorf("Expected %s, got %s", "ws://hello-world/devtools/browser/12345678-1234-1234-1234-123456789012", url)
	}
}

func TestGetNamespaceOfUser(t *testing.T) {
	controller := &Controller{}
	normalTeamID := uuid.New()

	tests := []struct {
		name      string
		user      *models.CreatedTeamAPIKey
		namespace string
	}{
		{
			name:      "nil user keeps legacy admin cluster scope",
			user:      nil,
			namespace: "",
		},
		{
			name:      "legacy key without team keeps admin cluster scope",
			user:      &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "legacy"},
			namespace: "",
		},
		{
			name:      "canonical admin team keeps cluster scope",
			user:      &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "admin", Team: models.AdminTeam()},
			namespace: "",
		},
		{
			name: "admin team name with mismatched id normalizes to cluster scope",
			user: &models.CreatedTeamAPIKey{
				ID:   uuid.New(),
				Name: "admin-name-mismatched-id",
				Team: &models.Team{
					ID:   uuid.New(),
					Name: models.AdminTeamName,
				},
			},
			namespace: "",
		},
		{
			name: "admin team id with mismatched name keeps cluster scope",
			user: &models.CreatedTeamAPIKey{
				ID:   uuid.New(),
				Name: "admin-id-mismatched-name",
				Team: &models.Team{
					ID:   models.AdminTeamID,
					Name: "not-admin",
				},
			},
			namespace: "",
		},
		{
			name: "normal team maps to team namespace",
			user: &models.CreatedTeamAPIKey{
				ID:   uuid.New(),
				Name: "team-a-user",
				Team: &models.Team{
					ID:   normalTeamID,
					Name: "team-a",
				},
			},
			namespace: "team-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.namespace, controller.getNamespaceOfUser(tt.user))
		})
	}
}
