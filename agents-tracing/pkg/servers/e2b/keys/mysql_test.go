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
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// Shared SQL regex constants used across multiple mock.ExpectQuery calls.
// loadKeyJoinPrefix is the common SELECT…JOIN preamble for key-load queries.
const loadKeyJoinPrefix = "SELECT team_api_keys\\.\\*, teams\\.uid AS team_uid, teams\\.name AS team_name FROM `team_api_keys` JOIN teams ON teams\\.id = team_api_keys\\.team_id AND teams\\.deleted_at IS NULL.*team_api_keys\\."

// loadKeyJoinByHashRegex matches key-load queries that filter by key_hash.
const loadKeyJoinByHashRegex = loadKeyJoinPrefix + "key_hash"

// loadKeyJoinByUIDRegex matches key-load queries that filter by uid.
const loadKeyJoinByUIDRegex = loadKeyJoinPrefix + "uid"

// listByOwnerRegex matches the subquery used by ListByOwnerTeam.
const listByOwnerRegex = "SELECT `created_at`,`uid`,`name`,`created_by_uid` FROM `team_api_keys` WHERE \\(team_id = \\(SELECT id FROM teams WHERE name = \\? AND deleted_at IS NULL LIMIT 1\\)\\) AND `team_api_keys`\\.`deleted_at` IS NULL"

// listTeamsRegex matches the EXISTS subquery used by ListTeams for admin users.
const listTeamsRegex = "SELECT `uid`,`name` FROM `teams` WHERE \\(EXISTS \\(SELECT 1 FROM team_api_keys WHERE team_api_keys\\.team_id = teams\\.id AND team_api_keys\\.deleted_at IS NULL\\)\\) AND `teams`\\.`deleted_at` IS NULL"

func newMockStorage(t *testing.T) (*mysqlKeyStorage, sqlmock.Sqlmock, func()) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	require.NoError(t, err)
	st := newMySQLKeyStorage(mysqlConfig{DSN: "dsn", AdminKey: "admin", Pepper: "pepper"})
	st.db = db
	return st, mock, func() { require.NoError(t, mock.ExpectationsWereMet()) }
}

// teamIDByName is a test helper that resolves a team name to its database ID.
func teamIDByName(t *testing.T, ctx context.Context, db *gorm.DB, name string) (uint, error) {
	t.Helper()
	var team TeamEntity
	if err := db.WithContext(ctx).Where(&TeamEntity{Name: name}).First(&team).Error; err != nil {
		return 0, fmt.Errorf("lookup team by name: %w", err)
	}
	return team.ID, nil
}

func TestMySQL_TableNamesAndHash(t *testing.T) {
	require.Equal(t, "teams", TeamEntity{}.TableName())
	require.Equal(t, "team_api_keys", TeamAPIKeyEntity{}.TableName())

	st := newMySQLKeyStorage(mysqlConfig{Pepper: "p1"})
	h1 := st.hashKey("k")
	h2 := st.hashKey("k")
	require.Equal(t, h1, h2)
	require.NotEqual(t, h1, newMySQLKeyStorage(mysqlConfig{Pepper: "p2"}).hashKey("k"))
}

func TestMySQL_InitBranches(t *testing.T) {
	t.Run("open error", func(t *testing.T) {
		oldOpen, oldMigrate := openMySQLDB, autoMigrateMySQLModels
		openMySQLDB = func(string) (*gorm.DB, error) { return nil, errors.New("open failed") }
		autoMigrateMySQLModels = func(context.Context, *gorm.DB) error { return nil }
		t.Cleanup(func() { openMySQLDB, autoMigrateMySQLModels = oldOpen, oldMigrate })

		err := newMySQLKeyStorage(mysqlConfig{DSN: "bad", AdminKey: "admin"}).Init(context.Background())
		require.Error(t, err)
	})

	t.Run("automigrate error", func(t *testing.T) {
		st, _, done := newMockStorage(t)
		defer done()
		oldOpen, oldMigrate := openMySQLDB, autoMigrateMySQLModels
		openMySQLDB = func(string) (*gorm.DB, error) { return st.db, nil }
		autoMigrateMySQLModels = func(context.Context, *gorm.DB) error { return errors.New("migrate failed") }
		t.Cleanup(func() { openMySQLDB, autoMigrateMySQLModels = oldOpen, oldMigrate })

		err := st.Init(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "automigrate")
	})

	t.Run("ensure admin team error", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		oldOpen, oldMigrate := openMySQLDB, autoMigrateMySQLModels
		openMySQLDB = func(string) (*gorm.DB, error) { return st.db, nil }
		autoMigrateMySQLModels = func(context.Context, *gorm.DB) error { return nil }
		t.Cleanup(func() { openMySQLDB, autoMigrateMySQLModels = oldOpen, oldMigrate })
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(errors.New("team failed"))
		require.Error(t, st.Init(context.Background()))
	})

	t.Run("ensure admin key error", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		oldOpen, oldMigrate := openMySQLDB, autoMigrateMySQLModels
		openMySQLDB = func(string) (*gorm.DB, error) { return st.db, nil }
		autoMigrateMySQLModels = func(context.Context, *gorm.DB) error { return nil }
		t.Cleanup(func() { openMySQLDB, autoMigrateMySQLModels = oldOpen, oldMigrate })
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(1, AdminTeamUID.String(), "admin"))
		mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(errors.New("lookup failed"))
		require.Error(t, st.Init(context.Background()))
	})

	t.Run("success", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		oldOpen, oldMigrate := openMySQLDB, autoMigrateMySQLModels
		openMySQLDB = func(string) (*gorm.DB, error) { return st.db, nil }
		autoMigrateMySQLModels = func(context.Context, *gorm.DB) error { return nil }
		t.Cleanup(func() { openMySQLDB, autoMigrateMySQLModels = oldOpen, oldMigrate })
		h := st.hashKey("admin")
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(1, AdminTeamUID.String(), "admin"))
		mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "key_hash", "name", "team_id", "deleted_at"}).AddRow(1, AdminKeyID.String(), h, "admin", 1, nil))
		mock.ExpectExec("UPDATE `team_api_keys` SET .*").WillReturnResult(sqlmock.NewResult(0, 1))
		require.NoError(t, st.Init(context.Background()))
	})

	t.Run("skip automigrate when disabled", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		st.cfg.DisableAutoMigrate = true

		oldOpen, oldMigrate := openMySQLDB, autoMigrateMySQLModels
		openMySQLDB = func(string) (*gorm.DB, error) { return st.db, nil }
		autoMigrateCalls := 0
		autoMigrateMySQLModels = func(context.Context, *gorm.DB) error {
			autoMigrateCalls++
			return errors.New("must not call automigrate")
		}
		t.Cleanup(func() { openMySQLDB, autoMigrateMySQLModels = oldOpen, oldMigrate })

		h := st.hashKey("admin")
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(1, AdminTeamUID.String(), "admin"))
		mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "key_hash", "name", "team_id", "deleted_at"}).AddRow(1, AdminKeyID.String(), h, "admin", 1, nil))
		mock.ExpectExec("UPDATE `team_api_keys` SET .*").WillReturnResult(sqlmock.NewResult(0, 1))

		require.NoError(t, st.Init(context.Background()))
		require.Zero(t, autoMigrateCalls)
	})
}

func TestMySQL_EnsureAdminTeamAndKey(t *testing.T) {
	t.Run("ensure admin team success", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(1, AdminTeamUID.String(), "admin"))
		id, err := st.ensureAdminTeam(context.Background())
		require.NoError(t, err)
		require.Equal(t, uint(1), id)
	})

	t.Run("ensure admin team error", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(errors.New("db error"))
		_, err := st.ensureAdminTeam(context.Background())
		require.Error(t, err)
	})

	t.Run("ensure admin key existing", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		h := st.hashKey("admin")
		mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(
			sqlmock.NewRows([]string{"id", "uid", "key_hash", "name", "team_id", "deleted_at"}).AddRow(1, AdminKeyID.String(), h, "admin", 1, nil),
		)
		mock.ExpectExec("UPDATE `team_api_keys` SET .*").WillReturnResult(sqlmock.NewResult(0, 1))
		require.NoError(t, st.ensureAdminKey(context.Background(), 1))
	})

	t.Run("ensure admin key create", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(gorm.ErrRecordNotFound)
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(1, 1))
		require.NoError(t, st.ensureAdminKey(context.Background(), 1))
	})

	t.Run("ensure admin key lookup error", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(errors.New("db down"))
		require.Error(t, st.ensureAdminKey(context.Background(), 1))
	})

	t.Run("ensure admin key create error", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(gorm.ErrRecordNotFound)
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnError(errors.New("insert fail"))
		require.Error(t, st.ensureAdminKey(context.Background(), 1))
	})
}

func TestMySQL_LoadByKeyAndID(t *testing.T) {
	st, mock, done := newMockStorage(t)
	defer done()
	now := time.Now()
	id := uuid.New()
	cb := uuid.New()
	teamID := uuid.New()
	hash := st.hashKey("raw")

	// DB hit for key, then cache hit for key/id.
	mock.ExpectQuery(loadKeyJoinByHashRegex).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid", "team_uid", "team_name"},
		).AddRow(1, now, now, nil, id.String(), "n", hash, 1, cb.String(), teamID.String(), "team-a"))
	got, ok := st.LoadByKey(context.Background(), "raw")
	require.True(t, ok)
	require.Equal(t, id, got.ID)
	require.Empty(t, got.Key)
	require.Equal(t, hash, got.KeyHash)
	require.NotNil(t, got.CreatedBy)
	require.Equal(t, cb, got.CreatedBy.ID)
	require.Equal(t, &models.Team{ID: teamID, Name: "team-a"}, got.Team)
	require.Equal(t, &models.Team{ID: teamID, Name: "team-a"}, st.teamCache.Get("team-a").Value())
	_, ok = st.LoadByKey(context.Background(), "raw")
	require.True(t, ok)
	_, ok = st.LoadByID(context.Background(), id.String())
	require.True(t, ok)

	mock.ExpectQuery(loadKeyJoinByUIDRegex).
		WillReturnError(gorm.ErrRecordNotFound)
	_, ok = st.LoadByID(context.Background(), uuid.NewString())
	require.False(t, ok)

	mock.ExpectQuery(loadKeyJoinByUIDRegex).
		WillReturnError(errors.New("load by id failed"))
	_, ok = st.LoadByID(context.Background(), uuid.NewString())
	require.False(t, ok)

	// direct DB success for LoadByID path.
	dbID := uuid.New()
	dbTeamID := uuid.New()
	dbHash := st.hashKey("id-success")
	mock.ExpectQuery(loadKeyJoinByUIDRegex).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid", "team_uid", "team_name"},
		).AddRow(2, now, now, nil, dbID.String(), "id-success", dbHash, 1, nil, dbTeamID.String(), "team-b"))
	gotByID, ok := st.LoadByID(context.Background(), dbID.String())
	require.True(t, ok)
	require.Equal(t, dbID, gotByID.ID)
	require.Empty(t, gotByID.Key)
	require.Equal(t, dbHash, gotByID.KeyHash)
	require.Equal(t, &models.Team{ID: dbTeamID, Name: "team-b"}, gotByID.Team)
	cachedByKey, ok := st.LoadByKey(context.Background(), "id-success")
	require.True(t, ok)
	require.Equal(t, dbID, cachedByKey.ID)

	mock.ExpectQuery(loadKeyJoinByHashRegex).
		WillReturnError(errors.New("db error"))
	_, ok = st.LoadByKey(context.Background(), "another")
	require.False(t, ok)

	// entity parse errors
	mock.ExpectQuery(loadKeyJoinByHashRegex).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid", "team_uid", "team_name"},
		).AddRow(1, now, now, nil, "bad", "n", st.hashKey("bad"), 1, nil, teamID.String(), "team-a"))
	_, ok = st.LoadByKey(context.Background(), "bad")
	require.False(t, ok)

	badTeamUIDKeyID := uuid.New()
	mock.ExpectQuery(loadKeyJoinByUIDRegex).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid", "team_uid", "team_name"},
		).AddRow(1, now, now, nil, badTeamUIDKeyID.String(), "n", st.hashKey("id2"), 1, nil, "bad", "team-bad"))
	_, ok = st.LoadByID(context.Background(), badTeamUIDKeyID.String())
	require.False(t, ok)

	missingTeamKeyID := uuid.New()
	mock.ExpectQuery(loadKeyJoinByUIDRegex).
		WillReturnError(gorm.ErrRecordNotFound)
	_, ok = st.LoadByID(context.Background(), missingTeamKeyID.String())
	require.False(t, ok)
}

func TestMySQL_CreateKeyBranches(t *testing.T) {
	userTeam := &models.Team{ID: uuid.New(), Name: "team-a"}
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: userTeam}

	t.Run("validation", func(t *testing.T) {
		st := newMySQLKeyStorage(mysqlConfig{Pepper: "pepper"})
		_, err := st.CreateKey(context.Background(), nil, CreateKeyOptions{Name: "n"})
		require.Error(t, err)
		_, err = st.CreateKey(context.Background(), user, CreateKeyOptions{})
		require.Error(t, err)
	})

	t.Run("team lookup error", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(errors.New("team error"))
		_, err := st.CreateKey(context.Background(), user, CreateKeyOptions{Name: "n"})
		require.Error(t, err)
	})

	t.Run("insert branches", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()

		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, userTeam.ID.String(), userTeam.Name))
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnError(errors.New("insert failed"))
		_, err := st.CreateKey(context.Background(), user, CreateKeyOptions{Name: "n"})
		require.Error(t, err)

		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, userTeam.ID.String(), userTeam.Name))
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(1, 1))
		key, err := st.CreateKey(context.Background(), user, CreateKeyOptions{Name: "n"})
		require.NoError(t, err)
		require.NotEmpty(t, key.Key)
		require.NotEmpty(t, key.KeyHash)
		require.NotNil(t, key.CreatedBy)
		require.Equal(t, user.ID, key.CreatedBy.ID)
		require.Equal(t, userTeam, key.Team)
	})

	t.Run("missing user team defaults to admin team", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()

		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, AdminTeamUID.String(), "admin"))
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(1, 1))
		key, err := st.CreateKey(context.Background(), &models.CreatedTeamAPIKey{ID: uuid.New()}, CreateKeyOptions{Name: "n"})
		require.NoError(t, err)
		require.Equal(t, models.AdminTeam(), key.Team)
	})

	t.Run("uniqueness exhaustion", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		oldGen := generateUUID
		fixed := uuid.MustParse("11111111-1111-1111-1111-111111111111")
		generateUUID = func() uuid.UUID { return fixed }
		t.Cleanup(func() { generateUUID = oldGen })

		// MySQL unique-index collision is intentionally capped to 5 retries.
		for i := 0; i < 5; i++ {
			mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, userTeam.ID.String(), userTeam.Name))
			mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnError(gorm.ErrDuplicatedKey)
		}
		_, err := st.CreateKey(context.Background(), user, CreateKeyOptions{Name: "n"})
		require.Error(t, err)
	})
}

func TestMySQL_CreateKeyCachesSanitizedClones(t *testing.T) {
	type testCase struct {
		name        string
		teamName    string
		expectError string
	}

	tests := []testCase{
		{
			name:     "create key returns raw key while cache returns sanitized clones",
			teamName: "team-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, mock, done := newMockStorage(t)
			defer done()

			team := &models.Team{ID: uuid.New(), Name: tt.teamName}
			user := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: team}
			mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(
				sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, team.ID.String(), team.Name),
			)
			mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(1, 1))

			created, err := st.CreateKey(context.Background(), user, CreateKeyOptions{Name: "n"})
			if tt.expectError != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, created.Key)
			require.NotEmpty(t, created.KeyHash)
			require.Empty(t, st.byKey.Get(created.KeyHash).Value().Key)
			require.Empty(t, st.byID.Get(created.ID.String()).Value().Key)

			byID, ok := st.LoadByID(context.Background(), created.ID.String())
			require.True(t, ok)
			require.Empty(t, byID.Key)
			require.NotEmpty(t, byID.KeyHash)
			require.Equal(t, tt.teamName, byID.Team.Name)
			require.NotNil(t, byID.CreatedBy)
			require.Equal(t, user.ID, byID.CreatedBy.ID)

			byKey, ok := st.LoadByKey(context.Background(), created.Key)
			require.True(t, ok)
			require.Empty(t, byKey.Key)
			require.NotEmpty(t, byKey.KeyHash)
			require.Equal(t, tt.teamName, byKey.Team.Name)

			byID.Team.Name = "mutated-team"
			byID.CreatedBy.ID = uuid.New()
			secondByID, ok := st.LoadByID(context.Background(), created.ID.String())
			require.True(t, ok)
			require.Equal(t, tt.teamName, secondByID.Team.Name)
			require.NotNil(t, secondByID.CreatedBy)
			require.Equal(t, user.ID, secondByID.CreatedBy.ID)
			require.Equal(t, team, st.teamCache.Get(tt.teamName).Value())

			created.Team.Name = "mutated-created-team"
			cachedTeam, ok, err := st.FindTeamByName(context.Background(), tt.teamName)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, tt.teamName, cachedTeam.Name)
		})
	}
}

func TestMySQL_ListByOwner(t *testing.T) {
	ownerKey := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Name: "owner-key",
		Team: &models.Team{Name: "my-team"},
	}
	now := time.Now()
	listCreator := uuid.New()
	listID := uuid.New()
	tests := []struct {
		name        string
		owner       *models.CreatedTeamAPIKey
		ctx         func() context.Context
		setupMock   func(sqlmock.Sqlmock)
		expectKeys  int
		expectError string
		validate    func(*testing.T, []*models.TeamAPIKey)
	}{
		{
			name:  "nil owner returns nil without error",
			owner: nil,
			ctx:   func() context.Context { return context.Background() },
		},
		{
			name:  "owner exists lists active team keys by team name subquery",
			owner: ownerKey,
			ctx:   func() context.Context { return context.Background() },
			setupMock: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(listByOwnerRegex).
					WillReturnRows(sqlmock.NewRows([]string{"created_at", "uid", "name", "created_by_uid"}).
						AddRow(now, listID.String(), "a", listCreator.String()).
						AddRow(now, "bad", "b", nil))
			},
			expectKeys: 1,
			validate: func(t *testing.T, keys []*models.TeamAPIKey) {
				t.Helper()
				require.Equal(t, listID, keys[0].ID)
				require.Equal(t, "a", keys[0].Name)
				require.Equal(t, now, keys[0].CreatedAt)
				require.NotNil(t, keys[0].CreatedBy)
				require.Equal(t, listCreator, keys[0].CreatedBy.ID)
			},
		},
		{
			name:  "owner with empty team falls back to admin team name",
			owner: &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "no-team"},
			ctx:   func() context.Context { return context.Background() },
			setupMock: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(listByOwnerRegex).
					WithArgs(models.AdminTeamName).
					WillReturnRows(sqlmock.NewRows([]string{"created_at", "uid", "name", "created_by_uid"}))
			},
		},
		{
			name:  "owner missing returns no keys without error",
			owner: ownerKey,
			ctx: func() context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				t.Cleanup(cancel)
				return ctx
			},
			setupMock: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(listByOwnerRegex).
					WillReturnRows(sqlmock.NewRows([]string{"created_at", "uid", "name", "created_by_uid"}))
			},
		},
		{
			name:  "list query error is returned",
			owner: ownerKey,
			ctx:   func() context.Context { return context.Background() },
			setupMock: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(listByOwnerRegex).
					WillReturnError(errors.New("list fail"))
			},
			expectError: "list keys by team",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, mock, done := newMockStorage(t)
			defer done()

			if tt.setupMock != nil {
				tt.setupMock(mock)
			}
			keys, err := st.ListByOwnerTeam(tt.ctx(), tt.owner)
			if tt.expectError != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			require.Len(t, keys, tt.expectKeys)
			if tt.validate != nil {
				tt.validate(t, keys)
			}
		})
	}
}

func TestMySQL_DeleteListTeamAndEntityHelpers(t *testing.T) {
	st, mock, done := newMockStorage(t)
	defer done()

	teamUID := uuid.New()

	now := time.Now()
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(9, teamUID.String(), "team"))
	teamID, err := teamIDByName(t, context.Background(), st.db, "team")
	require.NoError(t, err)
	require.Equal(t, uint(9), teamID)

	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(errors.New("team fail"))
	_, err = teamIDByName(t, context.Background(), st.db, "team")
	require.Error(t, err)

	_, err = st.loadCreatedKeyByUIDFromDB(context.Background(), "bad")
	require.Error(t, err)
	badCB := "bad"
	mock.ExpectQuery(loadKeyJoinByUIDRegex).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid", "team_uid", "team_name"},
		).AddRow(1, now, now, nil, uuid.NewString(), "bad-created-by", st.hashKey("bad-created-by"), 9, badCB, AdminTeamUID.String(), "admin"))
	out, err := st.loadCreatedKeyByUIDFromDB(context.Background(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, out.CreatedBy)

	mock.ExpectQuery(loadKeyJoinByUIDRegex).
		WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name", "team_id", "team_uid", "team_name"}).AddRow(1, uuid.NewString(), "ok", 9, AdminTeamUID.String(), "admin"))
	out, err = st.loadCreatedKeyByUIDFromDB(context.Background(), uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, "ok", out.Name)
	require.Empty(t, out.Key)
	require.Equal(t, models.AdminTeam(), out.Team)
}

func TestMySQL_DeleteKey(t *testing.T) {
	type testCase struct {
		name        string
		key         *models.CreatedTeamAPIKey
		setup       func(*mysqlKeyStorage, sqlmock.Sqlmock, *models.CreatedTeamAPIKey)
		verify      func(*testing.T, *mysqlKeyStorage, *models.CreatedTeamAPIKey)
		expectError string
		expectErrIs error
	}

	rawID := uuid.New()
	tests := []testCase{
		{
			name: "nil key delete success",
			key:  nil,
		},
		{
			name:        "admin key deletion returns undeletable error",
			key:         &models.CreatedTeamAPIKey{ID: AdminKeyID},
			expectError: ErrAdminKeyUndeletable.Error(),
			expectErrIs: ErrAdminKeyUndeletable,
		},
		{
			name: "key hash only deletes key",
			key:  &models.CreatedTeamAPIKey{ID: uuid.New(), KeyHash: "hash-present"},
			setup: func(st *mysqlKeyStorage, mock sqlmock.Sqlmock, key *models.CreatedTeamAPIKey) {
				st.cachePutKey(key)
				mock.ExpectExec("DELETE FROM `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(0, 1))
			},
			verify: func(t *testing.T, st *mysqlKeyStorage, key *models.CreatedTeamAPIKey) {
				t.Helper()
				require.Nil(t, st.byKey.Get(key.KeyHash))
				require.Nil(t, st.byID.Get(key.ID.String()))
			},
		},
		{
			name: "empty key hash and empty raw key selects key hash then deletes key",
			key:  &models.CreatedTeamAPIKey{ID: uuid.New()},
			setup: func(_ *mysqlKeyStorage, mock sqlmock.Sqlmock, _ *models.CreatedTeamAPIKey) {
				mock.ExpectQuery("SELECT `key_hash` FROM `team_api_keys`.*").
					WillReturnRows(sqlmock.NewRows([]string{"key_hash"}).AddRow("selected-hash"))
				mock.ExpectExec("DELETE FROM `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(0, 1))
			},
		},
		{
			name: "empty key hash and raw key computes hash then only deletes key",
			key:  &models.CreatedTeamAPIKey{ID: rawID, Key: "raw"},
			setup: func(st *mysqlKeyStorage, mock sqlmock.Sqlmock, key *models.CreatedTeamAPIKey) {
				st.cachePutKey(&models.CreatedTeamAPIKey{ID: key.ID, KeyHash: st.hashKey(key.Key)})
				mock.ExpectExec("DELETE FROM `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(0, 1))
			},
			verify: func(t *testing.T, st *mysqlKeyStorage, key *models.CreatedTeamAPIKey) {
				t.Helper()
				require.Nil(t, st.byKey.Get(st.hashKey(key.Key)))
				require.Nil(t, st.byID.Get(key.ID.String()))
			},
		},
		{
			name: "missing key returns nil",
			key:  &models.CreatedTeamAPIKey{ID: uuid.New(), KeyHash: "missing-hash"},
			setup: func(_ *mysqlKeyStorage, mock sqlmock.Sqlmock, _ *models.CreatedTeamAPIKey) {
				mock.ExpectExec("DELETE FROM `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(0, 0))
			},
		},
		{
			name: "delete failure returns error",
			key:  &models.CreatedTeamAPIKey{ID: uuid.New(), KeyHash: "delete-fail-hash"},
			setup: func(_ *mysqlKeyStorage, mock sqlmock.Sqlmock, _ *models.CreatedTeamAPIKey) {
				mock.ExpectExec("DELETE FROM `team_api_keys`.*").WillReturnError(errors.New("delete fail"))
			},
			expectError: "delete api-key",
		},
		{
			name: "deleting final non-admin team key does not delete team",
			key:  &models.CreatedTeamAPIKey{ID: uuid.New()},
			setup: func(_ *mysqlKeyStorage, mock sqlmock.Sqlmock, _ *models.CreatedTeamAPIKey) {
				mock.ExpectQuery("SELECT `key_hash` FROM `team_api_keys`.*").
					WillReturnRows(sqlmock.NewRows([]string{"key_hash"}).AddRow("final-hash"))
				mock.ExpectExec("DELETE FROM `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(0, 1))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, mock, done := newMockStorage(t)
			defer done()
			if tt.setup != nil {
				tt.setup(st, mock, tt.key)
			}

			err := st.DeleteKey(context.Background(), tt.key)
			if tt.expectError != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.expectError)
				if tt.expectErrIs != nil {
					require.ErrorIs(t, err, tt.expectErrIs)
				}
				return
			}
			require.NoError(t, err)
			if tt.verify != nil {
				tt.verify(t, st, tt.key)
			}
		})
	}
}

func TestMySQL_DeleteLoadedKeyInvalidatesByKeyCache(t *testing.T) {
	st, mock, done := newMockStorage(t)
	defer done()

	id := uuid.New()
	hash := st.hashKey("raw")
	now := time.Now()
	mock.ExpectQuery(loadKeyJoinByUIDRegex).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid", "team_uid", "team_name"},
		).AddRow(1, now, now, nil, id.String(), "n", hash, 1, nil, AdminTeamUID.String(), "admin"))
	apiKey, ok := st.LoadByID(context.Background(), id.String())
	require.True(t, ok)
	require.Empty(t, apiKey.Key)
	require.Equal(t, hash, apiKey.KeyHash)
	require.NotNil(t, st.byKey.Get(hash))

	mock.ExpectExec("DELETE FROM `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, st.DeleteKey(context.Background(), apiKey))
	require.Nil(t, st.byKey.Get(hash))
	require.Nil(t, st.byID.Get(id.String()))

	mock.ExpectQuery(loadKeyJoinByHashRegex).
		WillReturnError(gorm.ErrRecordNotFound)
	_, ok = st.LoadByKey(context.Background(), "raw")
	require.False(t, ok)
}

func TestMySQL_TeamLifecycle(t *testing.T) {
	t.Run("find and list active teams", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		teamID := uuid.New()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(
			sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, teamID.String(), "team-a"),
		)
		team, ok, err := st.FindTeamByName(context.Background(), "team-a")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, teamID, team.ID)
		require.Equal(t, "team-a", team.Name)

		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(gorm.ErrRecordNotFound)
		_, ok, err = st.FindTeamByName(context.Background(), "missing")
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("missing team is not cached", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		teamID := uuid.New()
		tests := []struct {
			name       string
			teamName   string
			setup      func(sqlmock.Sqlmock)
			expectTeam *models.Team
			expectOK   bool
		}{
			{
				name:     "first missing lookup returns false",
				teamName: "late-team",
				setup: func(mock sqlmock.Sqlmock) {
					mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(gorm.ErrRecordNotFound)
				},
			},
			{
				name:     "second lookup after row exists queries db and returns team",
				teamName: "late-team",
				setup: func(mock sqlmock.Sqlmock) {
					mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(
						sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(9, teamID.String(), "late-team"),
					)
				},
				expectTeam: &models.Team{ID: teamID, Name: "late-team"},
				expectOK:   true,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tt.setup(mock)
				team, ok, err := st.FindTeamByName(context.Background(), tt.teamName)
				require.NoError(t, err)
				require.Equal(t, tt.expectOK, ok)
				require.Equal(t, tt.expectTeam, team)
			})
		}
	})

	t.Run("find team caches first lookup", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		teamID := uuid.New()
		// Expect only ONE query: the first lookup hits the DB and primes the
		// cache; subsequent lookups must be served from teamCache.
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(
			sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(8, teamID.String(), "cached-team"),
		)

		for i := 0; i < 5; i++ {
			team, ok, err := st.FindTeamByName(context.Background(), "cached-team")
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, teamID, team.ID)
			require.Equal(t, "cached-team", team.Name)
		}
	})

	t.Run("create key creates team by name", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		admin := &models.CreatedTeamAPIKey{ID: AdminKeyID, Team: models.AdminTeam()}

		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(gorm.ErrRecordNotFound)
		mock.ExpectExec("INSERT INTO `teams`.*").WillReturnResult(sqlmock.NewResult(3, 1))
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(10, 1))
		key, err := st.CreateKey(context.Background(), admin, CreateKeyOptions{Name: "new-key", TeamName: "new-team"})
		require.NoError(t, err)
		require.Equal(t, "new-team", key.Team.Name)
		require.Equal(t, key.Team, st.teamCache.Get("new-team").Value())
	})
}

func TestMySQL_ListTeams(t *testing.T) {
	teamID := uuid.New()

	tests := []struct {
		name        string
		user        *models.CreatedTeamAPIKey
		setup       func(sqlmock.Sqlmock)
		expectTeams []*models.ListedTeam
		expectCache []*models.Team
		expectError string
	}{
		{
			name: "nil user returns nil without db query",
			user: nil,
		},
		{
			name: "non-admin user returns own team without db query",
			user: &models.CreatedTeamAPIKey{ID: uuid.New(), Team: &models.Team{ID: teamID, Name: "team-a"}},
			expectTeams: []*models.ListedTeam{
				{TeamID: teamID.String(), Name: "team-a", APIKey: "", IsDefault: true},
			},
		},
		{
			name: "admin team user queries active teams and marks admin default",
			user: &models.CreatedTeamAPIKey{ID: uuid.New(), Team: models.AdminTeam()},
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(listTeamsRegex).
					WillReturnRows(sqlmock.NewRows([]string{"uid", "name"}).
						AddRow(AdminTeamUID.String(), "admin").
						AddRow(teamID.String(), "team-a"))
			},
			expectTeams: []*models.ListedTeam{
				{TeamID: AdminTeamUID.String(), Name: "admin", APIKey: "", IsDefault: true},
				{TeamID: teamID.String(), Name: "team-a", APIKey: "", IsDefault: false},
			},
			expectCache: []*models.Team{
				{ID: AdminTeamUID, Name: "admin"},
				{ID: teamID, Name: "team-a"},
			},
		},
		{
			name: "admin query invalid team uid skips invalid row",
			user: &models.CreatedTeamAPIKey{ID: uuid.New(), Team: models.AdminTeam()},
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(listTeamsRegex).
					WillReturnRows(sqlmock.NewRows([]string{"uid", "name"}).
						AddRow("not-a-uuid", "bad-team").
						AddRow(teamID.String(), "team-a"))
			},
			expectTeams: []*models.ListedTeam{
				{TeamID: teamID.String(), Name: "team-a", APIKey: "", IsDefault: false},
			},
			expectCache: []*models.Team{
				{ID: teamID, Name: "team-a"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, mock, done := newMockStorage(t)
			defer done()
			if tt.setup != nil {
				tt.setup(mock)
			}

			teams, err := st.ListTeams(context.Background(), tt.user)
			if tt.expectError != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.expectTeams, teams)
			for _, team := range tt.expectCache {
				require.Equal(t, team, st.teamCache.Get(team.Name).Value())
			}
		})
	}
}

func TestMySQL_RunStopAndCachePut(t *testing.T) {
	st := newMySQLKeyStorage(mysqlConfig{Pepper: "pepper"})
	id := uuid.New()
	hash := st.hashKey("k")
	require.NotPanics(t, func() { st.cachePutKey(nil) })
	st.cachePutKey(&models.CreatedTeamAPIKey{ID: id, KeyHash: hash})
	require.NotNil(t, st.byKey.Get(hash))
	require.NotNil(t, st.byID.Get(id.String()))
	st.Run()
	st.Stop()
	require.NotPanics(t, func() { st.Stop() })
}

func TestCreatedTeamAPIKeyKeyHashIsNotSerialized(t *testing.T) {
	apiKey := &models.CreatedTeamAPIKey{
		ID:      uuid.New(),
		Key:     "raw",
		KeyHash: "hash",
		Name:    "n",
	}
	payload, err := json.Marshal(apiKey)
	require.NoError(t, err)
	require.Contains(t, string(payload), `"key":"raw"`)
	require.NotContains(t, string(payload), "KeyHash")
	require.NotContains(t, string(payload), "keyHash")
	require.NotContains(t, string(payload), "hash")
}
