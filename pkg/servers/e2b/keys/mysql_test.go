package keys

import (
	"context"
	"errors"
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
	got, ok := st.LoadByKey(context.Background(), "raw")
	require.True(t, ok)
	require.Equal(t, id, got.ID)
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
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "created_at", "updated_at", "deleted_at", "uid", "name", "key_hash", "team_id", "created_by_uid"},
	).AddRow(2, now, now, nil, dbID.String(), "id-success", st.hashKey("id-success"), 1, nil))
	gotByID, ok := st.LoadByID(context.Background(), dbID.String())
	require.True(t, ok)
	require.Equal(t, dbID, gotByID.ID)

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
	user := &models.CreatedTeamAPIKey{ID: uuid.New()}

	t.Run("validation", func(t *testing.T) {
		st := newMySQLKeyStorage(mysqlConfig{Pepper: "pepper"})
		_, err := st.CreateKey(context.Background(), nil, "n")
		require.Error(t, err)
		_, err = st.CreateKey(context.Background(), user, "")
		require.Error(t, err)
	})

	t.Run("team lookup error", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnError(errors.New("team error"))
		_, err := st.CreateKey(context.Background(), user, "n")
		require.Error(t, err)
	})

	t.Run("insert branches", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()

		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, AdminTeamUID.String(), "admin"))
		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnError(errors.New("insert failed"))
		mock.ExpectRollback()
		_, err := st.CreateKey(context.Background(), user, "n")
		require.Error(t, err)

		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, AdminTeamUID.String(), "admin"))
		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
		key, err := st.CreateKey(context.Background(), user, "n")
		require.NoError(t, err)
		require.NotEmpty(t, key.Key)
	})

	t.Run("uniqueness exhaustion", func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		oldGen := generateUUID
		fixed := uuid.MustParse("11111111-1111-1111-1111-111111111111")
		generateUUID = func() uuid.UUID { return fixed }
		t.Cleanup(func() { generateUUID = oldGen })

		mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(7, AdminTeamUID.String(), "admin"))
		// MySQL unique-index collision is intentionally capped to 5 retries.
		for i := 0; i < 5; i++ {
			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO `team_api_keys`.*").WillReturnError(gorm.ErrDuplicatedKey)
			mock.ExpectRollback()
		}
		_, err := st.CreateKey(context.Background(), user, "n")
		require.Error(t, err)
	})
}

func TestMySQL_DeleteListTeamAndEntityHelpers(t *testing.T) {
	st, mock, done := newMockStorage(t)
	defer done()

	require.NoError(t, st.DeleteKey(context.Background(), nil))

	id := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `team_api_keys` SET `deleted_at`=.*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, st.DeleteKey(context.Background(), &models.CreatedTeamAPIKey{ID: id, Key: "raw"}))

	id2 := uuid.New()
	mock.ExpectQuery("SELECT `key_hash` FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows([]string{"key_hash"}).AddRow("h"))
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `team_api_keys` SET `deleted_at`=.*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, st.DeleteKey(context.Background(), &models.CreatedTeamAPIKey{ID: id2}))

	id3 := uuid.New()
	mock.ExpectQuery("SELECT `key_hash` FROM `team_api_keys`.*").WillReturnError(gorm.ErrRecordNotFound)
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `team_api_keys` SET `deleted_at`=.*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, st.DeleteKey(context.Background(), &models.CreatedTeamAPIKey{ID: id3}))

	id4 := uuid.New()
	mock.ExpectQuery("SELECT `key_hash` FROM `team_api_keys`.*").WillReturnError(errors.New("prefetch fail"))
	require.Error(t, st.DeleteKey(context.Background(), &models.CreatedTeamAPIKey{ID: id4}))

	id5 := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `team_api_keys` SET `deleted_at`=.*").WillReturnError(errors.New("delete fail"))
	mock.ExpectRollback()
	require.Error(t, st.DeleteKey(context.Background(), &models.CreatedTeamAPIKey{ID: id5, Key: "raw"}))

	owner := uuid.New()
	now := time.Now()
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*uid.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "team_id"}).AddRow(1, owner.String(), 9))
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*team_id.*").WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "uid", "name", "team_id"}).
		AddRow(1, now, uuid.NewString(), "a", 9).
		AddRow(2, now, "bad", "b", 9))
	keys, err := st.ListByOwner(context.Background(), owner)
	require.NoError(t, err)
	require.Len(t, keys, 1)

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

	_, err = st.entityToCreated(&TeamAPIKeyEntity{UID: "bad"})
	require.Error(t, err)
	badCB := "bad"
	_, err = st.entityToCreated(&TeamAPIKeyEntity{UID: uuid.NewString(), CreatedByUID: &badCB})
	require.Error(t, err)
	out, err := st.entityToCreated(&TeamAPIKeyEntity{UID: uuid.NewString(), Name: "ok"})
	require.NoError(t, err)
	require.Equal(t, "ok", out.Name)
}

func TestMySQL_RunStopAndCachePut(t *testing.T) {
	st := newMySQLKeyStorage(mysqlConfig{Pepper: "pepper"})
	id := uuid.New()
	st.cachePut(&models.CreatedTeamAPIKey{ID: id, Key: "k"}, st.hashKey("k"))
	require.NotNil(t, st.byID.Get(id.String()))
	st.Run()
	st.Stop()
	require.NotPanics(t, func() { st.Stop() })
}
