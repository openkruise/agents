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
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type fakeKeyStorageForSubjects struct {
	keys    []*models.CreatedTeamAPIKey
	byID    map[string]*models.CreatedTeamAPIKey
	listErr error
}

func (f *fakeKeyStorageForSubjects) Init(context.Context) error { return nil }
func (f *fakeKeyStorageForSubjects) Run()                       {}
func (f *fakeKeyStorageForSubjects) Stop()                      {}
func (f *fakeKeyStorageForSubjects) LoadByKey(context.Context, string) (*models.CreatedTeamAPIKey, bool) {
	return nil, false
}
func (f *fakeKeyStorageForSubjects) LoadByID(_ context.Context, id string) (*models.CreatedTeamAPIKey, bool) {
	k, ok := f.byID[id]
	return k, ok
}
func (f *fakeKeyStorageForSubjects) CreateKey(context.Context, *models.CreatedTeamAPIKey, CreateKeyOptions) (*models.CreatedTeamAPIKey, error) {
	return nil, nil
}
func (f *fakeKeyStorageForSubjects) DeleteKey(context.Context, *models.CreatedTeamAPIKey) error {
	return nil
}
func (f *fakeKeyStorageForSubjects) ListByOwnerTeam(context.Context, *models.CreatedTeamAPIKey) ([]*models.TeamAPIKey, error) {
	return nil, nil
}
func (f *fakeKeyStorageForSubjects) ListLimited(context.Context) ([]*models.CreatedTeamAPIKey, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.keys, nil
}
func (f *fakeKeyStorageForSubjects) ListTeams(context.Context, *models.CreatedTeamAPIKey) ([]*models.ListedTeam, error) {
	return nil, nil
}
func (f *fakeKeyStorageForSubjects) FindTeamByName(context.Context, string) (*models.Team, bool, error) {
	return nil, false, nil
}

func TestQuotaSubjectListerListLimited(t *testing.T) {
	limitedID := uuid.New()
	unlimitedID := uuid.New()
	nilQuotaID := uuid.New()

	store := &fakeKeyStorageForSubjects{
		keys: []*models.CreatedTeamAPIKey{
			{ID: limitedID, QuotaSpec: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{
				Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeRunning, Limit: 5,
			}}}},
			{ID: unlimitedID, QuotaSpec: &quotaspec.QuotaSpec{}},
			{ID: nilQuotaID, QuotaSpec: nil},
			nil,
		},
	}
	lister := NewQuotaSubjectLister(store)

	subjects, err := lister.ListLimited(context.Background())
	require.NoError(t, err)
	require.Len(t, subjects, 1)
	assert.Equal(t, limitedID.String(), subjects[0].User)
	assert.NotNil(t, subjects[0].Quota)
	assert.True(t, subjects[0].Quota.IsLimited())
}

func TestQuotaSubjectListerListLimitedError(t *testing.T) {
	store := &fakeKeyStorageForSubjects{listErr: errors.New("db down")}
	lister := NewQuotaSubjectLister(store)

	_, err := lister.ListLimited(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")
}

func TestQuotaSubjectListerLoad(t *testing.T) {
	limitedID := uuid.New()
	store := &fakeKeyStorageForSubjects{
		byID: map[string]*models.CreatedTeamAPIKey{
			limitedID.String(): {
				ID: limitedID,
				QuotaSpec: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{
					Dimension: quotaspec.DimLimitsCPU, Scope: quotaspec.ScopeRunning, Limit: 4000,
				}}},
			},
			"unlimited": {
				ID:        uuid.New(),
				QuotaSpec: &quotaspec.QuotaSpec{},
			},
		},
	}
	lister := NewQuotaSubjectLister(store)

	tests := []struct {
		name   string
		user   string
		wantOK bool
	}{
		{"limited key found", limitedID.String(), true},
		{"unlimited key skipped", "unlimited", false},
		{"missing key", "nope", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subject, ok := lister.Load(context.Background(), tt.user)
			if tt.wantOK {
				require.True(t, ok)
				assert.Equal(t, tt.user, subject.User)
				assert.NotNil(t, subject.Quota)
			} else {
				assert.False(t, ok)
				assert.Equal(t, quotaspec.Subject{}, subject)
			}
		})
	}
}
