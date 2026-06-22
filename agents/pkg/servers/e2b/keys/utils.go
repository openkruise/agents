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
	"errors"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// TeamForKey returns the team for an API key, defaulting to AdminTeam for legacy keys without team information.
func TeamForKey(user *models.CreatedTeamAPIKey) *models.Team {
	if user == nil || user.Team == nil { // user will never be nil, just in case
		// Compatibility with old keys without team information
		return models.AdminTeam()
	}
	if user.Team.Name == models.AdminTeamName {
		return models.AdminTeam()
	}
	return user.Team
}

// validateCreateKeyOptions validates the inputs for CreateKey and resolves the team name.
// It returns the resolved team name or an error if validation fails.
func validateCreateKeyOptions(user *models.CreatedTeamAPIKey, opts CreateKeyOptions) (string, error) {
	if opts.Name == "" || user == nil {
		return "", errors.New("api-key name and user are required")
	}
	teamName := opts.TeamName
	if teamName == "" {
		teamName = TeamForKey(user).Name
	}
	if teamName == "" {
		return "", errors.New("api-key team name is required")
	}
	return teamName, nil
}

func cloneTeam(team *models.Team) *models.Team {
	if team == nil {
		return nil
	}
	return &models.Team{ID: team.ID, Name: team.Name}
}

func listedTeam(team *models.Team, isDefault bool) *models.ListedTeam {
	return &models.ListedTeam{
		TeamID:    team.ID.String(),
		Name:      team.Name,
		APIKey:    "",
		IsDefault: isDefault,
	}
}
