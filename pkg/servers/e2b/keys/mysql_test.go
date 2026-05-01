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
	"sync"
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
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `team_api_keys` SET .*").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
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
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `team_api_keys` SET .*").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

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
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `team_api_keys` SET .*").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		require.NoError(t, st.ensureAdminKey(context.Background(), 1))
	})

	t.Run("ensure admin key create", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(gorm.ErrRecordNotFound)
		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
		require.NoError(t, st.ensureAdminKey(context.Background(), 1))
	})

	t.Run("ensure admin key restores soft-deleted row", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		h := st.hashKey("admin")
		now := time.Now()
		mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(
			sqlmock.NewRows([]string{"id", "uid", "key_hash", "name", "team_id", "deleted_at"}).AddRow(1, AdminKeyID.String(), h, "admin", 1, now),
		)
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `team_api_keys` SET .*").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE `team_api_keys` SET `deleted_at`=.*").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
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
		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnError(errors.New("insert fail"))
		mock.ExpectRollback()
		require.Error(t, st.ensureAdminKey(context.Background(), 1))
	})
}

func TestMySQL_LoadByKeyAndID(t *testing.T) {
	st, mock, done := newMockStorage(t)
	defer done()
	now := time.Now()
	id := uuid.New()
	cb := uuid.New()
	hash := st.hashKey("raw")

	// DB hit for key, then cache hit for key/id.
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid"},
	).AddRow(1, now, now, nil, id.String(), "n", hash, 1, cb.String()))
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(1, AdminTeamUID.String(), "admin"))
	got, ok := st.LoadByKey(context.Background(), "raw")
	require.True(t, ok)
	require.Equal(t, id, got.ID)
	require.Empty(t, got.Key)
	require.Equal(t, hash, got.KeyHash)
	require.NotNil(t, got.CreatedBy)
	require.Equal(t, cb, got.CreatedBy.ID)
	require.Equal(t, models.AdminTeam(), got.Team)
	_, ok = st.LoadByKey(context.Background(), "raw")
	require.True(t, ok)
	_, ok = st.LoadByID(context.Background(), id.String())
	require.True(t, ok)

	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(gorm.ErrRecordNotFound)
	_, ok = st.LoadByID(context.Background(), uuid.NewString())
	require.False(t, ok)

	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(errors.New("load by id failed"))
	_, ok = st.LoadByID(context.Background(), uuid.NewString())
	require.False(t, ok)

	// direct DB success for LoadByID path.
	dbID := uuid.New()
	dbHash := st.hashKey("id-success")
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid"},
	).AddRow(2, now, now, nil, dbID.String(), "id-success", dbHash, 1, nil))
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(1, AdminTeamUID.String(), "admin"))
	gotByID, ok := st.LoadByID(context.Background(), dbID.String())
	require.True(t, ok)
	require.Equal(t, dbID, gotByID.ID)
	require.Empty(t, gotByID.Key)
	require.Equal(t, dbHash, gotByID.KeyHash)
	require.Equal(t, models.AdminTeam(), gotByID.Team)

	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(errors.New("db error"))
	_, ok = st.LoadByKey(context.Background(), "another")
	require.False(t, ok)

	// entity parse errors
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid"},
	).AddRow(1, now, now, nil, "bad", "n", st.hashKey("bad"), 1, nil))
	_, ok = st.LoadByKey(context.Background(), "bad")
	require.False(t, ok)

	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid"},
	).AddRow(1, now, now, nil, uuid.NewString(), "n", st.hashKey("id2"), 1, "bad"))
	_, ok = st.LoadByID(context.Background(), "id2")
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
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(errors.New("team error"))
		mock.ExpectRollback()
		_, err := st.CreateKey(context.Background(), user, CreateKeyOptions{Name: "n"})
		require.Error(t, err)
	})

	t.Run("insert branches", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()

		mock.ExpectBegin()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, userTeam.ID.String(), userTeam.Name))
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnError(errors.New("insert failed"))
		mock.ExpectRollback()
		_, err := st.CreateKey(context.Background(), user, CreateKeyOptions{Name: "n"})
		require.Error(t, err)

		mock.ExpectBegin()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, userTeam.ID.String(), userTeam.Name))
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
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

		mock.ExpectBegin()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, AdminTeamUID.String(), "admin"))
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
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
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, userTeam.ID.String(), userTeam.Name))
			mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnError(gorm.ErrDuplicatedKey)
			mock.ExpectRollback()
		}
		_, err := st.CreateKey(context.Background(), user, CreateKeyOptions{Name: "n"})
		require.Error(t, err)
	})
}

func TestMySQL_DeleteListTeamAndEntityHelpers(t *testing.T) {
	st, mock, done := newMockStorage(t)
	defer done()

	require.NoError(t, st.DeleteKey(context.Background(), nil))

	id := uuid.New()
	teamUID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "uid", "name", "key_hash", "team_id"},
	).AddRow(1, id.String(), "n", "h", 9))
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(9, teamUID.String(), "team"))
	mock.ExpectExec("UPDATE `team_api_keys` SET `deleted_at`=.*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("UPDATE `teams` SET `deleted_at`=.*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, st.DeleteKey(context.Background(), &models.CreatedTeamAPIKey{ID: id, Key: "raw"}))

	adminID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "uid", "name", "key_hash", "team_id"},
	).AddRow(2, adminID.String(), "admin", "admin-h", 1))
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(1, AdminTeamUID.String(), "admin"))
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectRollback()
	require.ErrorIs(t, st.DeleteKey(context.Background(), &models.CreatedTeamAPIKey{ID: adminID}), ErrLastAdminKey)

	id3 := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(gorm.ErrRecordNotFound)
	mock.ExpectCommit()
	require.NoError(t, st.DeleteKey(context.Background(), &models.CreatedTeamAPIKey{ID: id3}))

	id4 := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(errors.New("prefetch fail"))
	mock.ExpectRollback()
	require.Error(t, st.DeleteKey(context.Background(), &models.CreatedTeamAPIKey{ID: id4}))

	id5 := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "uid", "name", "key_hash", "team_id"},
	).AddRow(5, id5.String(), "n", "h5", 9))
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(9, teamUID.String(), "team"))
	mock.ExpectExec("UPDATE `team_api_keys` SET `deleted_at`=.*").WillReturnError(errors.New("delete fail"))
	mock.ExpectRollback()
	require.Error(t, st.DeleteKey(context.Background(), &models.CreatedTeamAPIKey{ID: id5, Key: "raw"}))

	owner := uuid.New()
	now := time.Now()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*uid.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "team_id"}).AddRow(1, owner.String(), 9))
	listCreator := uuid.New()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*team_id.*").WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "uid", "name", "team_id", "created_by_uid"}).
		AddRow(1, now, uuid.NewString(), "a", 9, listCreator.String()).
		AddRow(2, now, "bad", "b", 9, nil))
	keys, err := st.ListByOwner(context.Background(), owner)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.NotNil(t, keys[0].CreatedBy)
	require.Equal(t, listCreator, keys[0].CreatedBy.ID)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*uid.*").WillReturnError(gorm.ErrRecordNotFound)
	keys, err = st.ListByOwner(ctx, owner)
	require.NoError(t, err)
	require.Nil(t, keys)

	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*uid.*").WillReturnError(errors.New("owner fail"))
	_, err = st.ListByOwner(context.Background(), owner)
	require.Error(t, err)

	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*uid.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "team_id"}).AddRow(1, owner.String(), 9))
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*team_id.*").WillReturnError(errors.New("list fail"))
	_, err = st.ListByOwner(context.Background(), owner)
	require.Error(t, err)

	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(9, owner.String(), "team"))
	teamID, err := st.teamIDByUID(context.Background(), owner.String())
	require.NoError(t, err)
	require.Equal(t, uint(9), teamID)

	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(errors.New("team fail"))
	_, err = st.teamIDByUID(context.Background(), owner.String())
	require.Error(t, err)

	_, err = st.loadCreatedKeyFromDB(context.Background(), &TeamAPIKeyEntity{UID: "bad"})
	require.Error(t, err)
	badCB := "bad"
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid"},
	).AddRow(1, now, now, nil, uuid.NewString(), "bad-created-by", st.hashKey("bad-created-by"), 9, badCB))
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(9, AdminTeamUID.String(), "admin"))
	out, err := st.loadCreatedKeyFromDB(context.Background(), &TeamAPIKeyEntity{UID: uuid.NewString(), CreatedByUID: &badCB})
	require.NoError(t, err)
	require.Nil(t, out.CreatedBy)

	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name", "team_id"}).AddRow(1, uuid.NewString(), "ok", 9))
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(9, AdminTeamUID.String(), "admin"))
	out, err = st.loadCreatedKeyFromDB(context.Background(), &TeamAPIKeyEntity{UID: uuid.NewString()})
	require.NoError(t, err)
	require.Equal(t, "ok", out.Name)
	require.Empty(t, out.Key)
	require.Equal(t, models.AdminTeam(), out.Team)
}

func TestMySQL_DeleteLoadedKeyInvalidatesByKeyCache(t *testing.T) {
	st, mock, done := newMockStorage(t)
	defer done()

	id := uuid.New()
	hash := st.hashKey("raw")
	now := time.Now()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid"},
	).AddRow(1, now, now, nil, id.String(), "n", hash, 1, nil))
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(1, AdminTeamUID.String(), "admin"))
	apiKey, ok := st.LoadByID(context.Background(), id.String())
	require.True(t, ok)
	require.Empty(t, apiKey.Key)
	require.Equal(t, hash, apiKey.KeyHash)
	require.NotNil(t, st.byKey.Get(hash))

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid"},
	).AddRow(1, now, now, nil, id.String(), "n", hash, 1, nil))
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(1, AdminTeamUID.String(), "admin"))
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectExec("UPDATE `team_api_keys` SET `deleted_at`=.*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, st.DeleteKey(context.Background(), apiKey))
	require.Nil(t, st.byKey.Get(hash))
	require.Nil(t, st.byID.Get(id.String()))

	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnError(gorm.ErrRecordNotFound)
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

		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).
			AddRow(1, AdminTeamUID.String(), "admin").
			AddRow(7, teamID.String(), "team-a"))
		teams, err := st.ListTeams(context.Background(), &models.CreatedTeamAPIKey{ID: AdminKeyID, Team: models.AdminTeam()})
		require.NoError(t, err)
		require.Len(t, teams, 2)
		require.Empty(t, teams[0].APIKey)

		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(
			sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, teamID.String(), "team-a"),
		)
		teams, err = st.ListTeams(context.Background(), &models.CreatedTeamAPIKey{ID: uuid.New(), Team: &models.Team{ID: teamID, Name: "team-a"}})
		require.NoError(t, err)
		require.Len(t, teams, 1)
		require.Equal(t, "team-a", teams[0].Name)
		require.True(t, teams[0].IsDefault)
	})

	t.Run("find team uses cache and singleflight", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		teamID := uuid.New()

		// Expect only ONE query even for multiple calls
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(
			sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(8, teamID.String(), "cached-team"),
		)

		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				team, ok, err := st.FindTeamByName(context.Background(), "cached-team")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, teamID, team.ID)
				require.Equal(t, "cached-team", team.Name)
			}()
		}
		wg.Wait()

		// Try again sequentially, should also hit cache
		team, ok, err := st.FindTeamByName(context.Background(), "cached-team")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, teamID, team.ID)
	})

	t.Run("create key creates or restores team by name", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		admin := &models.CreatedTeamAPIKey{ID: AdminKeyID, Team: models.AdminTeam()}

		mock.ExpectBegin()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(gorm.ErrRecordNotFound)
		mock.ExpectExec("INSERT INTO `teams`.*").WillReturnResult(sqlmock.NewResult(3, 1))
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(10, 1))
		mock.ExpectCommit()
		key, err := st.CreateKey(context.Background(), admin, CreateKeyOptions{Name: "new-key", TeamName: "new-team"})
		require.NoError(t, err)
		require.Equal(t, "new-team", key.Team.Name)

		restoredTeamID := uuid.New()
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(
			sqlmock.NewRows([]string{"id", "uid", "name", "deleted_at"}).AddRow(4, restoredTeamID.String(), "restored-team", time.Now()),
		)
		mock.ExpectExec("UPDATE `teams` SET `deleted_at`=.*").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(11, 1))
		mock.ExpectCommit()
		key, err = st.CreateKey(context.Background(), admin, CreateKeyOptions{Name: "restore-key", TeamName: "restored-team"})
		require.NoError(t, err)
		require.Equal(t, restoredTeamID, key.Team.ID)
		require.Equal(t, "restored-team", key.Team.Name)
	})
}

func TestMySQL_RunStopAndCachePut(t *testing.T) {
	st := newMySQLKeyStorage(mysqlConfig{Pepper: "pepper"})
	id := uuid.New()
	hash := st.hashKey("k")
	st.cachePut(&models.CreatedTeamAPIKey{ID: id, KeyHash: hash})
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
