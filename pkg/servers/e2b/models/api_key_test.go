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
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestNewSystemUser(t *testing.T) {
	tests := []struct {
		name string
		id   uuid.UUID
	}{
		{
			name: "standard system user",
			id:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		},
		{
			name: "nil uuid",
			id:   uuid.Nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NewSystemUser(tt.name, tt.id)

			assert.NotNil(t, result)
			assert.Equal(t, tt.id, result.ID)
			assert.Equal(t, tt.name, result.Name)
			assert.NotNil(t, result.Team)
			assert.Equal(t, AdminTeamID, result.Team.ID)
			assert.Equal(t, AdminTeamName, result.Team.Name)
		})
	}
}
