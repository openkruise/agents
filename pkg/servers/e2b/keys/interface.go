package keys

import (
	"context"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type KeyStorage interface {
	Init(ctx context.Context) error
	Run()
	LoadByKey(key string) (*models.CreatedTeamAPIKey, bool)
	LoadByID(id string) (*models.CreatedTeamAPIKey, bool)
	CreateKey(ctx context.Context, user *models.CreatedTeamAPIKey, name string) (*models.CreatedTeamAPIKey, error)
	DeleteKey(ctx context.Context, key *models.CreatedTeamAPIKey) error
	ListByOwner(owner uuid.UUID) []*models.TeamAPIKey
}
