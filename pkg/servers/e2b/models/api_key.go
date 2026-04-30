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

package models

import (
	"time"

	"github.com/google/uuid"
)

const AdminTeamName = "admin"

var AdminTeamID = uuid.MustParse("550e8400-e29b-41d4-a716-446655449999")

// Team represents an E2B team.
type Team struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func AdminTeam() *Team {
	return &Team{
		ID:   AdminTeamID,
		Name: AdminTeamName,
	}
}

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
	KeyHash   string                   `json:"-"`
	Mask      IdentifierMaskingDetails `json:"mask"`
	Name      string                   `json:"name"`
	CreatedBy *TeamUser                `json:"createdBy"`
	Team      *Team                    `json:"team,omitempty"`
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
