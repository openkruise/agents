package models

import (
	"time"

	"github.com/google/uuid"
)

// TeamUser represents a user in a team
type TeamUser struct {
	Email string    `json:"email"`
	ID    uuid.UUID `json:"id"`
}

// IdentifierMaskingDetails contains details for masking identifiers
type IdentifierMaskingDetails struct {
	MaskedValuePrefix string `json:"maskedValuePrefix"`
	MaskedValueSuffix string `json:"maskedValueSuffix"`
	Prefix            string `json:"prefix"`
	ValueLength       int    `json:"valueLength"`
}

// CreatedTeamAPIKey represents a newly created team API key
type CreatedTeamAPIKey struct {
	CreatedAt time.Time                `json:"createdAt"`
	ID        uuid.UUID                `json:"id"`
	Key       string                   `json:"key"`
	Mask      IdentifierMaskingDetails `json:"mask"`
	Name      string                   `json:"name"`
	CreatedBy *TeamUser                `json:"createdBy"`
	LastUsed  *time.Time               `json:"lastUsed"`
}

// TeamAPIKey represents a team API key
type TeamAPIKey struct {
	CreatedAt time.Time                `json:"createdAt"`
	ID        uuid.UUID                `json:"id"`
	Mask      IdentifierMaskingDetails `json:"mask"`
	Name      string                   `json:"name"`
	CreatedBy *TeamUser                `json:"createdBy"`
	LastUsed  *time.Time               `json:"lastUsed"`
}

// NewTeamAPIKey represents a request to create a new team API key
type NewTeamAPIKey struct {
	Name string `json:"name"`
}

// UpdateTeamAPIKey represents a request to update a team API key
type UpdateTeamAPIKey struct {
	Name string `json:"name"`
}
