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

package opensandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// fakeKeyStorage is a minimal keys.KeyStorage that only backs LoadByKey, which
// is the sole method CheckApiKey consults. The remaining methods return zero
// values: they are unreachable from the auth path and exist only to satisfy the
// interface. A local fake is used because the keys package exposes no importable
// test double (its stub lives in a _test.go file).
type fakeKeyStorage struct {
	byKey map[string]*models.CreatedTeamAPIKey
}

func (f *fakeKeyStorage) Init(context.Context) error { return nil }
func (f *fakeKeyStorage) Run()                       {}
func (f *fakeKeyStorage) Stop()                      {}

func (f *fakeKeyStorage) LoadByKey(_ context.Context, key string) (*models.CreatedTeamAPIKey, bool) {
	user, ok := f.byKey[key]
	return user, ok
}

func (f *fakeKeyStorage) LoadByID(_ context.Context, id string) (*models.CreatedTeamAPIKey, bool) {
	for _, user := range f.byKey {
		if user.ID.String() == id {
			return user, true
		}
	}
	return nil, false
}

func (f *fakeKeyStorage) CreateKey(context.Context, *models.CreatedTeamAPIKey, keys.CreateKeyOptions) (*models.CreatedTeamAPIKey, error) {
	return nil, nil
}
func (f *fakeKeyStorage) DeleteKey(context.Context, *models.CreatedTeamAPIKey) error { return nil }
func (f *fakeKeyStorage) ListByOwnerTeam(context.Context, *models.CreatedTeamAPIKey) ([]*models.TeamAPIKey, error) {
	return nil, nil
}
func (f *fakeKeyStorage) ListLimited(context.Context) ([]*models.CreatedTeamAPIKey, error) {
	return nil, nil
}
func (f *fakeKeyStorage) ListTeams(context.Context, *models.CreatedTeamAPIKey) ([]*models.ListedTeam, error) {
	return nil, nil
}
func (f *fakeKeyStorage) FindTeamByName(context.Context, string) (*models.Team, bool, error) {
	return nil, false, nil
}

func TestCheckApiKey(t *testing.T) {
	validKey := "valid-test-key"
	validUser := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Name: "tester",
		Team: &models.Team{ID: uuid.New(), Name: "team-a"},
	}

	tests := []struct {
		name       string
		keyStore   keys.KeyStorage
		header     string
		setHeader  bool
		wantErr    bool
		wantCode   int
		wantUserID uuid.UUID
	}{
		{
			name:       "nil key store runs as anonymous admin",
			keyStore:   nil,
			setHeader:  false,
			wantUserID: keys.AdminKeyID,
		},
		{
			name:       "valid key resolves user",
			keyStore:   &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{validKey: validUser}},
			header:     validKey,
			setHeader:  true,
			wantUserID: validUser.ID,
		},
		{
			name:      "invalid key is unauthorized",
			keyStore:  &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{validKey: validUser}},
			header:    "wrong-key",
			setHeader: true,
			wantErr:   true,
			wantCode:  http.StatusUnauthorized,
		},
		{
			name:      "missing key is unauthorized",
			keyStore:  &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{validKey: validUser}},
			setHeader: false,
			wantErr:   true,
			wantCode:  http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, RoutePrefix+"/sandboxes", nil)
			if tt.setHeader {
				r.Header.Set(HeaderOpenSandboxAPIKey, tt.header)
			}

			mw := CheckApiKey(tt.keyStore)
			ctx, apiErr := mw(context.Background(), r)

			if tt.wantErr {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.wantCode, apiErr.Code)
				return
			}
			require.Nil(t, apiErr)
			user := GetUserFromContext(ctx)
			require.NotNil(t, user)
			assert.Equal(t, tt.wantUserID, user.ID)
		})
	}
}

func TestGetUserFromContext(t *testing.T) {
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "tester"}

	tests := []struct {
		name string
		ctx  context.Context
		want *models.CreatedTeamAPIKey
	}{
		{name: "absent user returns nil", ctx: context.Background(), want: nil},
		{
			name: "present user is returned",
			ctx:  context.WithValue(context.Background(), userContextKey, user),
			want: user,
		},
		{
			name: "wrong type returns nil",
			ctx:  context.WithValue(context.Background(), userContextKey, "not-a-user"),
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, GetUserFromContext(tt.ctx))
		})
	}
}

func TestNamespaceOfUser(t *testing.T) {
	tests := []struct {
		name string
		user *models.CreatedTeamAPIKey
		want string
	}{
		{
			name: "admin team maps to cluster scope",
			user: &models.CreatedTeamAPIKey{ID: uuid.New(), Team: models.AdminTeam()},
			want: "",
		},
		{
			name: "named team maps to its namespace",
			user: &models.CreatedTeamAPIKey{ID: uuid.New(), Team: &models.Team{ID: uuid.New(), Name: "team-a"}},
			want: "team-a",
		},
		{
			name: "legacy key without team defaults to admin",
			user: &models.CreatedTeamAPIKey{ID: uuid.New()},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, NamespaceOfUser(tt.user))
		})
	}
}
