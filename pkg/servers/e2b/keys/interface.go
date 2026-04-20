package keys

import (
	"context"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// KeyStorage abstracts API key persistence. Implementations must be safe for concurrent use.
type KeyStorage interface {
	Init(ctx context.Context) error
	Run()
	Stop()
	LoadByKey(ctx context.Context, key string) (*models.CreatedTeamAPIKey, bool)
	LoadByID(ctx context.Context, id string) (*models.CreatedTeamAPIKey, bool)
	CreateKey(ctx context.Context, user *models.CreatedTeamAPIKey, name string) (*models.CreatedTeamAPIKey, error)
	DeleteKey(ctx context.Context, key *models.CreatedTeamAPIKey) error
	ListByOwner(ctx context.Context, owner uuid.UUID) ([]*models.TeamAPIKey, error)
}
