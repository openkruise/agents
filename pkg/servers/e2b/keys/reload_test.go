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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/configreload"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

const (
	reloadTestDSN    = "user:pass@tcp(localhost:3306)/keys?parseTime=true"
	reloadTestAdminA = "administrator-A"
	reloadTestAdminB = "administrator-B"
)

// TestMySQLReloadValidation pins every input a running storage refuses. The
// fresh rows are first snapshots, where the dynamic checks run instead of the
// identity comparison against the accepted configuration.
func TestMySQLReloadValidation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		changes map[string]string
		fresh   bool
	}{
		{"pepper", map[string]string{configreload.Pepper: "different"}, false},
		{"database", map[string]string{configreload.DSN: "user:pass@tcp(localhost:3306)/other?parseTime=true"}, false},
		{"option", map[string]string{configreload.DSN: "user:pass@tcp(localhost:3306)/keys?parseTime=false"}, false},
		{"connection attributes", map[string]string{configreload.DSN: reloadTestDSN + "&connectionAttributes=application%3AB"}, false},
		{"connection attributes and admin", map[string]string{configreload.DSN: reloadTestDSN + "&connectionAttributes=application%3AB", configreload.AdminKey: "rotated-admin"}, false},
		{"invalid DSN", map[string]string{configreload.DSN: "broken-dsn"}, false},
		{"empty DSN", map[string]string{configreload.DSN: ""}, true},
		{"empty pepper", map[string]string{configreload.Pepper: ""}, true},
		{"short ASCII admin", map[string]string{configreload.AdminKey: "12345678901"}, false},
		{"short unicode admin", map[string]string{configreload.AdminKey: "短多字节凭据"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, _, done := newMockStorage(t)
			defer done()
			st.cfg.DSN, st.cfg.AdminKey = reloadTestDSN, "accepted-admin"
			st.initialized = !tt.fresh
			before := st.db
			// Rejected candidates must not dial or write, even if they also
			// contain explicit admin rotation intent.
			oldOpen := openMySQLDB
			openMySQLDB = func(context.Context, string) (*gorm.DB, error) {
				t.Error("rejected candidate opened a connection")
				return nil, errors.New("unexpected connection")
			}
			t.Cleanup(func() { openMySQLDB = oldOpen })
			if strings.HasPrefix(tt.name, "connection attributes") {
				st.cfg.DSN += "&connectionAttributes=application%3AA"
			}
			result, err := st.Reload(t.Context(), tt.changes)
			require.NoError(t, err)
			require.Equal(t, configreload.Rejected, result)
			require.Same(t, before, st.db)
			require.Equal(t, "accepted-admin", st.cfg.AdminKey)
		})
	}
}

func expectAdminReload(mock sqlmock.Sqlmock, storedHash string) {
	mock.ExpectQuery("SELECT .*FROM `teams`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "name"}).AddRow(1, AdminTeamUID.String(), "admin"))
	mock.ExpectQuery("SELECT .*FROM `team_api_keys`.*").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "key_hash", "name", "team_id", "deleted_at"}).AddRow(1, AdminKeyID.String(), storedHash, "admin", 1, nil))
}

func TestMySQLReloadTwoReplicasRevokeAdminAfterConvergence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, mock, done := newMockStorage(t)
		defer done()
		sqlDB, err := first.db.DB()
		require.NoError(t, err)
		defer func() {
			mock.ExpectClose()
			require.NoError(t, sqlDB.Close())
		}()
		first.cfg.DSN, first.cfg.AdminKey = reloadTestDSN, reloadTestAdminA
		first.initialized = true
		second := newMySQLKeyStorage(first.cfg)
		// Independent dispatchers and caches access the same database. Script the
		// shared row's exact history; synctest advances TTLs without wall-clock waits.
		oldOpen, oldMigrate := openMySQLDB, autoMigrateMySQLModels
		openMySQLDB = func(context.Context, string) (*gorm.DB, error) { return first.db, nil }
		autoMigrateMySQLModels = func(context.Context, *gorm.DB) error { return nil }
		t.Cleanup(func() { openMySQLDB, autoMigrateMySQLModels = oldOpen, oldMigrate })
		dispatcher := func(storage *mysqlKeyStorage) *configreload.Dispatcher {
			return configreload.NewDispatcher([]configreload.Component{{Name: "keys", Keys: []string{configreload.AdminKey}, Reload: storage.Reload}})
		}
		d1, d2 := dispatcher(first), dispatcher(second)
		apply := func(d *configreload.Dispatcher, admin string) {
			t.Helper()
			require.NoError(t, d.Apply(t.Context(), map[string]string{configreload.AdminKey: admin}))
		}
		adminRow := func(hash string) *sqlmock.Rows {
			return sqlmock.NewRows([]string{"uid", "key_hash", "team_uid", "team_name"}).
				AddRow(AdminKeyID.String(), hash, AdminTeamUID.String(), "admin")
		}
		hashA, hashB := first.hashKey(reloadTestAdminA), first.hashKey(reloadTestAdminB)
		expectAdminReload(mock, hashA)
		mock.ExpectExec(updateAdminKeyRegex).WillReturnResult(sqlmock.NewResult(0, 1))
		apply(d1, reloadTestAdminB)

		// A delayed replica's initial snapshot restores A after replica 1 accepts B.
		expectAdminReload(mock, hashB)
		mock.ExpectExec(updateAdminKeyRegex).WillReturnResult(sqlmock.NewResult(0, 1))
		apply(d2, reloadTestAdminA)
		mock.ExpectQuery(loadKeyJoinByHashRegex).WithArgs(hashA, 1).WillReturnRows(adminRow(hashA))
		_, found := first.LoadByKey(t.Context(), reloadTestAdminA)
		require.True(t, found)
		byKeyItem, byIDItem := first.byKey.Get(hashA), first.byID.Get(AdminKeyID.String())
		require.NotNil(t, byKeyItem, "administrator authentication must use the cache")
		require.NotNil(t, byIDItem)
		keyExpiry, idExpiry := byKeyItem.ExpiresAt(), byIDItem.ExpiresAt()

		expectAdminReload(mock, hashA)
		mock.ExpectExec(updateAdminKeyRegex).WillReturnResult(sqlmock.NewResult(0, 1))
		apply(d2, reloadTestAdminB)
		// Both dispatchers confirm B. Traffic may hit stale A until its fixed TTL,
		// but neither index may extend that deadline, and hits must issue no SQL.
		for range 3 {
			time.Sleep(keyCacheTTL / 4)
			_, found = first.LoadByKey(t.Context(), reloadTestAdminA)
			require.True(t, found)
			byID, found := first.LoadByID(t.Context(), AdminKeyID.String())
			require.True(t, found)
			require.Equal(t, hashA, byID.KeyHash)
			require.Equal(t, keyExpiry, byKeyItem.ExpiresAt(), "key hits must not renew revoked credentials")
			require.Equal(t, idExpiry, byIDItem.ExpiresAt(), "ID hits must not renew revoked credentials")
		}
		time.Sleep(keyCacheTTL/4 + time.Nanosecond)
		for range 2 {
			mock.ExpectQuery(loadKeyJoinByHashRegex).WithArgs(hashA, 1).WillReturnRows(sqlmock.NewRows([]string{"uid", "key_hash", "team_uid", "team_name"}))
			_, found = first.LoadByKey(t.Context(), reloadTestAdminA)
			require.False(t, found, "revoked admin authenticated beyond the fixed TTL")
		}
		mock.ExpectQuery(loadKeyJoinByUIDRegex).WithArgs(AdminKeyID.String(), 1).WillReturnRows(adminRow(hashB))
		byID, found := first.LoadByID(t.Context(), AdminKeyID.String())
		require.True(t, found)
		require.Equal(t, hashB, byID.KeyHash)
		// The ID lookup populated B in both indexes; authentication must hit cache.
		_, found = first.LoadByKey(t.Context(), reloadTestAdminB)
		require.True(t, found)
		// Direct store changes remain observable without rewriting configured B.
		hashDirect := first.hashKey("direct")
		mock.ExpectQuery(loadKeyJoinByHashRegex).WithArgs(hashDirect, 1).WillReturnRows(adminRow(hashDirect))
		_, found = first.LoadByKey(t.Context(), "direct")
		require.True(t, found)
		time.Sleep(keyCacheTTL + time.Nanosecond)
		mock.ExpectQuery(loadKeyJoinByHashRegex).WithArgs(hashB, 1).WillReturnRows(sqlmock.NewRows([]string{"uid"}))
		_, found = first.LoadByKey(t.Context(), reloadTestAdminB)
		require.False(t, found)
		mock.ExpectQuery(loadKeyJoinByHashRegex).WithArgs(hashDirect, 1).WillReturnError(errors.New("database unavailable"))
		_, found = first.LoadByKey(t.Context(), "direct")
		require.False(t, found)
	})
}

func TestMySQLReloadTwoReplicasRetryAdminConflictWithLatestInput(t *testing.T) {
	first, mock, done := newMockStorage(t)
	defer done()
	first.cfg.DSN, first.cfg.AdminKey, first.initialized = reloadTestDSN, "initial-admin", true
	second := newMySQLKeyStorage(first.cfg)
	second.db, second.initialized = first.db, true
	dispatcher := func(storage *mysqlKeyStorage) *configreload.Dispatcher {
		return configreload.NewDispatcher([]configreload.Component{{Name: "keys", Keys: []string{configreload.AdminKey}, Reload: storage.Reload}})
	}
	d1, d2 := dispatcher(first), dispatcher(second)
	hashOriginal := first.hashKey("initial-admin")
	// Replica 2 has read the original row and is about to write stale input A.
	// Commit B on replica 1 at that exact boundary, without timing or goroutines.
	require.NoError(t, first.db.Callback().Update().Before("gorm:update").Register("rotate-before-stale-admin-write", func(db *gorm.DB) {
		values, ok := db.Statement.Dest.(map[string]any)
		if ok && db.Statement.Table == "team_api_keys" && values["key_hash"] == first.hashKey(reloadTestAdminA) {
			require.NoError(t, d1.Apply(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminB}))
		}
	}))
	expectAdminReload(mock, hashOriginal) // replica 2 reads before replica 1 commits
	expectAdminReload(mock, hashOriginal)
	mock.ExpectExec(updateAdminKeyRegex).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(updateAdminKeyRegex).WillReturnResult(sqlmock.NewResult(0, 0)) // conditional write loses
	require.Error(t, d2.Apply(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminA}))
	require.Equal(t, reloadTestAdminB, first.cfg.AdminKey)
	require.Equal(t, "initial-admin", second.cfg.AdminKey, "a lost write cannot confirm stale input")

	// Retry uses the current snapshot B, not the failed A. B is already durable,
	// so confirming it must issue no UPDATE and must release startup readiness.
	expectAdminReload(mock, first.hashKey(reloadTestAdminB))
	require.NoError(t, d2.Apply(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminB}))
	require.NoError(t, d1.Wait(t.Context()))
	require.NoError(t, d2.Wait(t.Context()))
	require.Equal(t, reloadTestAdminB, second.cfg.AdminKey)
}

func TestMySQLReloadUncertainAdminWrite(t *testing.T) {
	var output bytes.Buffer
	ctx := klog.NewContext(t.Context(), funcr.New(func(_, message string) { output.WriteString(message) }, funcr.Options{}))
	st, mock, done := newMockStorage(t)
	defer done()
	st.cfg.DSN, st.cfg.AdminKey = reloadTestDSN, reloadTestAdminA
	st.initialized = true
	st.cachePutKey(&models.CreatedTeamAPIKey{ID: AdminKeyID, KeyHash: st.hashKey(reloadTestAdminA), Team: models.AdminTeam()})
	expectAdminReload(mock, st.hashKey(reloadTestAdminA))
	mock.ExpectExec(updateAdminKeyRegex).WillReturnError(errors.New("response lost: sensitive backend detail"))
	_, err := st.Reload(ctx, map[string]string{configreload.AdminKey: reloadTestAdminB})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "sensitive")
	require.Contains(t, output.String(), "mysql key storage reload candidate preparation failed")
	require.NotContains(t, output.String(), "sensitive backend detail")
	require.Equal(t, reloadTestAdminA, st.cfg.AdminKey)

	// The failed B may have committed. Returning to confirmed input A remains
	// explicit unresolved write intent and must inspect/repair the actual row.
	expectAdminReload(mock, st.hashKey(reloadTestAdminB))
	mock.ExpectExec(updateAdminKeyRegex).WillReturnResult(sqlmock.NewResult(0, 1))
	result, err := st.Reload(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminA})
	require.NoError(t, err)
	require.Equal(t, configreload.Accepted, result)
	require.Nil(t, st.cacheGetByKey(st.hashKey(reloadTestAdminA)))

	// DSN-only changes must not restore the unchanged configured admin.
	result, err = st.Reload(t.Context(), map[string]string{configreload.DSN: reloadTestDSN})
	require.NoError(t, err)
	require.Equal(t, configreload.Accepted, result)
}

func TestKeyStorageReloadSanitizesBackendErrors(t *testing.T) {
	const sensitive = "sensitive DSN=password admin-key pepper"
	for _, stage := range []string{"mysql open", "mysql schema", "secret persistence", "secret observation"} {
		t.Run(stage, func(t *testing.T) {
			var output bytes.Buffer
			ctx := klog.NewContext(t.Context(), funcr.New(func(_, message string) { output.WriteString(message) }, funcr.Options{}))
			var err error
			if strings.HasPrefix(stage, "mysql") {
				storage, mock, done := newMockStorage(t)
				defer done()
				storage.cfg.DSN = reloadTestDSN
				db := storage.db
				storage.db = nil
				oldOpen, oldMigrate := openMySQLDB, autoMigrateMySQLModels
				t.Cleanup(func() { openMySQLDB, autoMigrateMySQLModels = oldOpen, oldMigrate })
				openMySQLDB = func(context.Context, string) (*gorm.DB, error) {
					if stage == "mysql open" {
						return nil, errors.New(sensitive)
					}
					return db, nil
				}
				autoMigrateMySQLModels = func(context.Context, *gorm.DB) error { return errors.New(sensitive) }
				if stage == "mysql schema" {
					mock.ExpectClose()
				}
				_, err = storage.Reload(ctx, map[string]string{configreload.AdminKey: reloadTestAdminA})
			} else {
				storage, api := newSecretStorageForTestWithKey(t, reloadTestAdminA, nil)
				if stage == "secret persistence" {
					storage.Client = &patchHookClient{Client: api, patchHook: func(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
						return errors.New(sensitive)
					}}
				} else {
					storage.reloadReader = &getHookClient{Client: api, getHook: func(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
						return errors.New(sensitive)
					}}
				}
				_, err = storage.Reload(ctx, map[string]string{configreload.AdminKey: reloadTestAdminA})
			}
			require.Error(t, err)
			require.NotContains(t, err.Error(), sensitive)
			require.Contains(t, output.String(), "failed", "the actual component failure log must be captured")
			require.Contains(t, output.String(), `"category"="backend"`)
			require.NotContains(t, output.String(), sensitive)
		})
	}
}

func TestMySQLReloadDrainsOperationsBeforePublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, mock, done := newMockStorage(t)
		defer done()
		st.cfg.DSN, st.cfg.AdminKey = reloadTestDSN+"&connectionAttributes=application%3AA", reloadTestAdminA
		st.initialized = true
		oldDB := st.db
		oldSQL, err := oldDB.DB()
		require.NoError(t, err)
		queryEntered, releaseQuery := make(chan struct{}), make(chan struct{})
		var enteredOnce sync.Once
		require.NoError(t, st.db.Callback().Query().Before("gorm:query").Register("block-old-auth", func(db *gorm.DB) {
			if db.Statement.Table == "team_api_keys" && len(db.Statement.Selects) > 0 && strings.Contains(db.Statement.Selects[0], "team_api_keys.*") {
				enteredOnce.Do(func() { close(queryEntered) })
				<-releaseQuery
			}
		}))
		mock.ExpectQuery(loadKeyJoinByHashRegex).WillReturnRows(sqlmock.NewRows([]string{"uid", "key_hash", "team_uid", "team_name"}).AddRow(AdminKeyID.String(), st.hashKey(reloadTestAdminA), AdminTeamUID.String(), "admin"))
		queryDone := make(chan struct{})
		go func() { defer close(queryDone); st.LoadByKey(t.Context(), reloadTestAdminA) }()
		<-queryEntered

		next, nextMock, nextDone := newMockStorage(t)
		defer nextDone()
		nextSQL, err := next.db.DB()
		require.NoError(t, err)
		defer func() {
			nextMock.ExpectClose()
			require.NoError(t, nextSQL.Close())
		}()
		oldOpen := openMySQLDB
		prepared := make(chan string, 1)
		openMySQLDB = func(_ context.Context, dsn string) (*gorm.DB, error) { prepared <- dsn; return next.db, nil }
		t.Cleanup(func() { openMySQLDB = oldOpen })
		// These expectations forbid migration/admin bootstrap on a credential-only reload.
		mock.ExpectClose()
		reloadDone := make(chan error, 1)
		wantDSN := "new:password@tcp(localhost:3306)/keys?parseTime=true&connectionAttributes=application%3AA"
		go func() {
			_, err := st.Reload(t.Context(), map[string]string{configreload.DSN: wantDSN})
			reloadDone <- err
		}()
		require.Equal(t, wantDSN, <-prepared)
		synctest.Wait()
		select {
		case err := <-reloadDone:
			t.Fatalf("published with an operation still running: %v", err)
		default:
		}
		require.NoError(t, oldSQL.PingContext(t.Context()), "old connection must remain open while used")
		cached := &models.CreatedTeamAPIKey{ID: AdminKeyID, KeyHash: st.hashKey("cached"), Team: models.AdminTeam()}
		st.cachePutKey(cached)
		st.cachePutTeam(cached.Team)
		for _, tt := range []struct {
			name           string
			call           func(context.Context) error
			lookup         bool
			defaultTimeout bool
		}{
			{name: "LoadByKey", lookup: true, call: func(ctx context.Context) error {
				_, found := st.LoadByKey(ctx, "cached")
				require.False(t, found)
				return nil
			}},
			{name: "LoadByID", lookup: true, call: func(ctx context.Context) error {
				_, found := st.LoadByID(ctx, AdminKeyID.String())
				require.False(t, found)
				return nil
			}},
			{name: "CreateKey", call: func(ctx context.Context) error {
				_, err := st.CreateKey(ctx, cached, CreateKeyOptions{Name: "queued"})
				return err
			}},
			{name: "DeleteKey", call: func(ctx context.Context) error { return st.DeleteKey(ctx, cached) }},
			{name: "ListByOwnerTeam", defaultTimeout: true, call: func(ctx context.Context) error {
				_, err := st.ListByOwnerTeam(ctx, cached)
				return err
			}},
			{name: "ListLimited", call: func(ctx context.Context) error {
				_, err := st.ListLimited(ctx)
				return err
			}},
			{name: "ListTeams", defaultTimeout: true, call: func(ctx context.Context) error {
				_, err := st.ListTeams(ctx, cached)
				return err
			}},
			{name: "FindTeamByName", call: func(ctx context.Context) error {
				_, _, err := st.FindTeamByName(ctx, models.AdminTeamName)
				return err
			}},
		} {
			ctx := t.Context()
			budget := defaultListTimeout
			if !tt.defaultTimeout {
				var cancel context.CancelFunc
				budget = time.Second
				ctx, cancel = context.WithTimeout(ctx, budget)
				defer cancel()
			}
			started := time.Now()
			operationDone := make(chan error, 1)
			go func() { operationDone <- tt.call(ctx) }()
			synctest.Wait()
			select {
			case err := <-operationDone:
				t.Fatalf("%s overtook queued publication: %v", tt.name, err)
			default:
			}
			err := <-operationDone
			if tt.lookup {
				require.NoError(t, err, tt.name)
			} else {
				require.ErrorIs(t, err, context.DeadlineExceeded, tt.name)
			}
			require.Equal(t, budget, time.Since(started), "%s must include queueing in its timeout", tt.name)
		}
		close(releaseQuery)
		<-queryDone
		require.NoError(t, <-reloadDone)
		require.Same(t, next.db, st.db)
		require.Nil(t, st.cacheGetByKey(st.hashKey(reloadTestAdminA)), "old query cannot fill the published cache")
		nextMock.ExpectQuery(loadKeyJoinByHashRegex).WillReturnError(gorm.ErrRecordNotFound)
		_, found := st.LoadByKey(t.Context(), reloadTestAdminA)
		require.False(t, found)
		require.Error(t, oldSQL.PingContext(t.Context()), "retired connection must be closed")
	})
}

func TestMySQLPublicationCancellationPreservesState(t *testing.T) {
	for _, newConnection := range []bool{false, true} {
		t.Run(fmt.Sprintf("newConnection=%v", newConnection), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st, mock, done := newMockStorage(t)
				defer done()
				oldSQL, err := st.db.DB()
				require.NoError(t, err)
				defer func() {
					mock.ExpectClose()
					require.NoError(t, oldSQL.Close())
				}()
				st.initialized = true
				cached := &models.CreatedTeamAPIKey{ID: AdminKeyID, KeyHash: st.hashKey(reloadTestAdminA), Team: models.AdminTeam()}
				st.cachePutKey(cached)
				st.cachePutTeam(cached.Team)
				beforeDB, beforeConfig := st.db, st.cfg
				candidate := &mysqlKeyStorage{db: st.db, cfg: st.cfg}
				candidate.cfg.AdminKey = reloadTestAdminB
				if newConnection {
					next, nextMock, nextDone := newMockStorage(t)
					defer nextDone()
					candidate.db = next.db
					nextMock.ExpectClose()
				}
				// Model an in-flight operation while publication waits to drain.
				require.NoError(t, st.operationGate.Acquire(t.Context(), 1))
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				require.ErrorIs(t, st.publish(ctx, candidate, st.db), context.DeadlineExceeded)
				st.operationGate.Release(1)
				require.Same(t, beforeDB, st.db)
				require.Equal(t, beforeConfig, st.cfg)
				require.True(t, st.initialized)
				require.NoError(t, oldSQL.PingContext(t.Context()), "canceled publication must preserve the old connection")
				if newConnection {
					nextSQL, err := candidate.db.DB()
					require.NoError(t, err)
					require.Error(t, nextSQL.PingContext(t.Context()), "unpublished candidate must be closed")
				}
				_, found := st.LoadByKey(t.Context(), reloadTestAdminA)
				require.True(t, found, "canceled publication must preserve the key cache and admit operations again")
				require.NotNil(t, st.cacheGetByID(AdminKeyID.String()))
				require.NotNil(t, st.teamCache.Get(models.AdminTeamName))
			})
		})
	}
}

func TestSecretReloadUncertainWriteAndIndependentObservation(t *testing.T) {
	storage, api := newSecretStorageForTestWithKey(t, reloadTestAdminA, nil)
	_, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminA})
	require.NoError(t, err)
	storage.Client = &patchHookClient{Client: api, patchHook: func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if err := api.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		return errors.New("write completed but response lost")
	}}
	_, err = storage.Reload(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminB})
	require.Error(t, err)
	require.Equal(t, reloadTestAdminA, storage.AdminKey)
	require.True(t, adminKeyStateSatisfied(getSecretForTest(t, api), reloadTestAdminB))
	storage.Client = api
	_, err = storage.Reload(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminA})
	require.NoError(t, err)
	_, found := storage.LoadByKey(t.Context(), reloadTestAdminB)
	require.False(t, found)

	// Direct store changes are observation, not a new configuration write.
	stored := getSecretForTest(t, api)
	key := &models.CreatedTeamAPIKey{ID: AdminKeyID, Key: "direct", Name: "admin", Team: models.AdminTeam()}
	raw, err := marshalStoredAPIKey(key)
	require.NoError(t, err)
	stored.Data[AdminKeyID.String()] = raw
	require.NoError(t, api.Update(t.Context(), stored))
	result, err := storage.Reload(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, configreload.NotNecessary, result)
	_, found = storage.LoadByKey(t.Context(), "direct")
	require.False(t, found, "configuration reload must not observe unrelated store changes")
	require.NoError(t, storage.refresh(t.Context(), api))
	_, found = storage.LoadByKey(t.Context(), "direct")
	require.True(t, found)
	require.True(t, adminKeyStateSatisfied(getSecretForTest(t, api), "direct"))
}

func TestSecretReloadWaitsForInformerObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		storage, api := newSecretStorageForTestWithKey(t, reloadTestAdminA, nil)
		_, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminA})
		require.NoError(t, err)
		stale := &staleSecretReadClient{Client: api, secret: getSecretForTest(t, api)}
		storage.Client, storage.Cache = stale, newStubCache()
		storage.Run()
		defer storage.Stop()
		finished := make(chan error, 1)
		go func() {
			_, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminB})
			finished <- err
		}()
		synctest.Wait()
		require.True(t, adminKeyStateSatisfied(getSecretForTest(t, api), reloadTestAdminB))
		select {
		case err := <-finished:
			t.Fatalf("accepted before cache observation: %v", err)
		default:
		}
		_, found := storage.LoadByKey(t.Context(), reloadTestAdminA)
		require.True(t, found)
		stale.ClearSecret()
		require.NoError(t, <-finished)
		_, found = storage.LoadByKey(t.Context(), reloadTestAdminB)
		require.True(t, found)
		_, found = storage.LoadByKey(t.Context(), reloadTestAdminA)
		require.False(t, found)
	})
}

func TestMySQLReloadCandidateFailurePreservesRuntime(t *testing.T) {
	st, _, done := newMockStorage(t)
	defer done()
	st.cfg.DSN, st.cfg.AdminKey, st.initialized = reloadTestDSN, reloadTestAdminA, true
	previous := st.db
	oldOpen := openMySQLDB
	openMySQLDB = func(ctx context.Context, _ string) (*gorm.DB, error) { <-ctx.Done(); return nil, ctx.Err() }
	t.Cleanup(func() { openMySQLDB = oldOpen })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err := st.Reload(ctx, map[string]string{configreload.DSN: strings.Replace(reloadTestDSN, "user:pass", "new:password", 1)})
	require.Error(t, err)
	require.Same(t, previous, st.db)
	require.Equal(t, reloadTestDSN, st.cfg.DSN)
}

func TestMySQLOpenCancellationDuringHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { require.NoError(t, listener.Close()) }()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = openMySQLDB(ctx, "user:secret@tcp("+listener.Addr().String()+")/keys")
	require.Error(t, err)
	require.Less(t, time.Since(started), time.Second)
	select {
	case conn := <-accepted:
		require.NoError(t, conn.Close())
	case <-time.After(time.Second):
		t.Fatal("driver never connected to the test server")
	}
}

func TestMySQLReloadFirstInitialization(t *testing.T) {
	storage, mock, done := newMockStorage(t)
	defer done()
	storage.cfg.DSN, storage.cfg.AdminKey = reloadTestDSN, reloadTestAdminA
	oldOpen, oldMigrate := openMySQLDB, autoMigrateMySQLModels
	opened, migrated := 0, 0
	openMySQLDB = func(context.Context, string) (*gorm.DB, error) { opened++; return storage.db, nil }
	autoMigrateMySQLModels = func(context.Context, *gorm.DB) error { migrated++; return nil }
	t.Cleanup(func() { openMySQLDB, autoMigrateMySQLModels = oldOpen, oldMigrate })
	expectAdminReload(mock, storage.hashKey(reloadTestAdminA))
	result, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: reloadTestAdminA, configreload.DSN: reloadTestDSN, configreload.Pepper: "pepper"})
	require.NoError(t, err)
	require.Equal(t, configreload.Accepted, result)
	_, err = storage.Reload(t.Context(), map[string]string{configreload.DSN: reloadTestDSN})
	require.NoError(t, err)
	require.Equal(t, 1, opened)
	require.Equal(t, 1, migrated)
}

func TestSecretReloadAdminLength(t *testing.T) {
	for _, tt := range []struct {
		name string
		key  string
		want configreload.Result
	}{
		{"short ASCII", "12345678901", configreload.Rejected},
		{"short Unicode", strings.Repeat("短", 11), configreload.Rejected},
		{"minimum ASCII", "123456789012", configreload.Accepted},
		{"minimum Unicode", strings.Repeat("短", 12), configreload.Accepted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := marshalStoredAPIKey(&models.CreatedTeamAPIKey{ID: AdminKeyID, Key: "persisted-admin", Name: "admin", Team: models.AdminTeam()})
			require.NoError(t, err)
			storage, api := newSecretStorageForTestWithKey(t, "", map[string][]byte{AdminKeyID.String(): raw})
			result, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: tt.key})
			require.NoError(t, err)
			require.Equal(t, tt.want, result)
			persisted := "persisted-admin"
			if tt.want == configreload.Accepted {
				require.Equal(t, tt.key, storage.AdminKey)
				persisted = tt.key
				_, found := storage.LoadByKey(t.Context(), persisted)
				require.True(t, found)
			}
			require.True(t, adminKeyStateSatisfied(getSecretForTest(t, api), persisted))
		})
	}
}
