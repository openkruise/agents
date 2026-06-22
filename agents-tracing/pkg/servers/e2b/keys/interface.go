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
	"context"
	"errors"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

var (
	ErrAdminKeyUndeletable = errors.New("the well-known admin api-key cannot be deleted")
)

// CreateKeyOptions describes the target key display name and optional target team.
type CreateKeyOptions struct {
	Name     string
	TeamName string
}

// KeyStorage abstracts API key persistence. Implementations must be safe for concurrent use.
type KeyStorage interface {
	Init(ctx context.Context) error
	Run()
	Stop()
	LoadByKey(ctx context.Context, key string) (*models.CreatedTeamAPIKey, bool)
	LoadByID(ctx context.Context, id string) (*models.CreatedTeamAPIKey, bool)
	CreateKey(ctx context.Context, key *models.CreatedTeamAPIKey, opts CreateKeyOptions) (*models.CreatedTeamAPIKey, error)
	DeleteKey(ctx context.Context, key *models.CreatedTeamAPIKey) error
	ListByOwnerTeam(ctx context.Context, owner *models.CreatedTeamAPIKey) ([]*models.TeamAPIKey, error)
	ListTeams(ctx context.Context, user *models.CreatedTeamAPIKey) ([]*models.ListedTeam, error)
	FindTeamByName(ctx context.Context, teamName string) (*models.Team, bool, error)
}
