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

package keys

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestTeamForKey(t *testing.T) {
	tests := []struct {
		name       string
		user       *models.CreatedTeamAPIKey
		expectTeam *models.Team
	}{
		{
			name:       "nil user defaults to admin team",
			user:       nil,
			expectTeam: models.AdminTeam(),
		},
		{
			name: "missing team defaults to admin team",
			user: &models.CreatedTeamAPIKey{
				ID: uuid.New(),
			},
			expectTeam: models.AdminTeam(),
		},
		{
			name: "admin team name is normalized to canonical admin team",
			user: &models.CreatedTeamAPIKey{
				ID: uuid.New(),
				Team: &models.Team{
					Name: models.AdminTeamName,
				},
			},
			expectTeam: models.AdminTeam(),
		},
		{
			name: "non-admin team is preserved",
			user: &models.CreatedTeamAPIKey{
				ID: uuid.New(),
				Team: &models.Team{
					ID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
					Name: "team-a",
				},
			},
			expectTeam: &models.Team{
				ID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
				Name: "team-a",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TeamForKey(tt.user)
			assert.Equal(t, tt.expectTeam, got)
		})
	}
}
