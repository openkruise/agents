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

const reloadTestDSN = "user:pass@tcp(localhost:3306)/keys?parseTime=true"

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
		{"short unicode admin", map[string]string{configreload.AdminKey: "短多字节凭据"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, _, done := newMockStorage(t)
			defer done()
			st.cfg.DSN, st.cfg.AdminKey, st.cfg.AdminKeyMinLength = reloadTestDSN, "accepted-admin", 12
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

func TestMySQLReloadUncertainAdminWrite(t *testing.T) {
	var output bytes.Buffer
	ctx := klog.NewContext(t.Context(), funcr.New(func(_, message string) { output.WriteString(message) }, funcr.Options{}))
	st, mock, done := newMockStorage(t)
	defer done()
	st.cfg.DSN, st.cfg.AdminKey = reloadTestDSN, "A"
	st.initialized = true
	st.cachePutKey(&models.CreatedTeamAPIKey{ID: AdminKeyID, KeyHash: st.hashKey("A"), Team: models.AdminTeam()})
	expectAdminReload(mock, st.hashKey("A"))
	mock.ExpectExec(updateAdminKeyRegex).WillReturnError(errors.New("response lost: sensitive backend detail"))
	_, err := st.Reload(ctx, map[string]string{configreload.AdminKey: "B"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "sensitive")
	require.Contains(t, output.String(), "mysql key storage reload candidate preparation failed")
	require.NotContains(t, output.String(), "sensitive backend detail")
	require.Equal(t, "A", st.cfg.AdminKey)

	// The failed B may have committed. Returning to confirmed input A remains
	// explicit unresolved write intent and must inspect/repair the actual row.
	expectAdminReload(mock, st.hashKey("B"))
	mock.ExpectExec(updateAdminKeyRegex).WillReturnResult(sqlmock.NewResult(0, 1))
	result, err := st.Reload(t.Context(), map[string]string{configreload.AdminKey: "A"})
	require.NoError(t, err)
	require.Equal(t, configreload.Accepted, result)
	require.Nil(t, st.cacheGetByKey(st.hashKey("A")))

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
				_, err = storage.Reload(ctx, map[string]string{configreload.AdminKey: "A"})
			} else {
				storage, api := newSecretStorageForTestWithKey(t, "A", nil)
				if stage == "secret persistence" {
					storage.Client = &patchHookClient{Client: api, patchHook: func(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
						return errors.New(sensitive)
					}}
				} else {
					storage.reloadReader = &getHookClient{Client: api, getHook: func(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
						return errors.New(sensitive)
					}}
				}
				_, err = storage.Reload(ctx, map[string]string{configreload.AdminKey: "A"})
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
	st, mock, done := newMockStorage(t)
	defer done()
	st.cfg.DSN, st.cfg.AdminKey = reloadTestDSN+"&connectionAttributes=application%3AA", "A"
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
	mock.ExpectQuery(loadKeyJoinByHashRegex).WillReturnRows(sqlmock.NewRows([]string{"uid", "key_hash", "team_uid", "team_name"}).AddRow(AdminKeyID.String(), st.hashKey("A"), AdminTeamUID.String(), "admin"))
	queryDone := make(chan struct{})
	go func() { defer close(queryDone); st.LoadByKey(t.Context(), "A") }()
	<-queryEntered

	next, nextMock, nextDone := newMockStorage(t)
	defer nextDone()
	oldOpen := openMySQLDB
	prepared := make(chan struct{})
	openMySQLDB = func(context.Context, string) (*gorm.DB, error) { close(prepared); return next.db, nil }
	t.Cleanup(func() { openMySQLDB = oldOpen })
	// These expectations forbid migration/admin bootstrap on a credential-only reload.
	mock.ExpectClose()
	reloadDone := make(chan error, 1)
	go func() {
		_, err := st.Reload(t.Context(), map[string]string{configreload.DSN: strings.Replace(st.cfg.DSN, "user:pass", "new:password", 1)})
		reloadDone <- err
	}()
	<-prepared
	select {
	case err := <-reloadDone:
		t.Fatalf("published with an operation still running: %v", err)
	default:
	}
	require.NoError(t, oldSQL.PingContext(t.Context()), "old connection must remain open while used")
	close(releaseQuery)
	<-queryDone
	require.NoError(t, <-reloadDone)
	require.Same(t, next.db, st.db)
	require.Nil(t, st.cacheGetByKey(st.hashKey("A")), "old query cannot fill the published cache")
	nextMock.ExpectQuery(loadKeyJoinByHashRegex).WillReturnError(gorm.ErrRecordNotFound)
	_, found := st.LoadByKey(t.Context(), "A")
	require.False(t, found)
	require.Error(t, oldSQL.PingContext(t.Context()), "retired connection must be closed")
}

func TestSecretReloadUncertainWriteAndIndependentObservation(t *testing.T) {
	storage, api := newSecretStorageForTestWithKey(t, "A", nil)
	_, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: "A"})
	require.NoError(t, err)
	storage.Client = &patchHookClient{Client: api, patchHook: func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if err := api.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		return errors.New("write completed but response lost")
	}}
	_, err = storage.Reload(t.Context(), map[string]string{configreload.AdminKey: "B"})
	require.Error(t, err)
	require.Equal(t, "A", storage.AdminKey)
	require.True(t, adminKeyStateSatisfied(getSecretForTest(t, api), "B"))
	storage.Client = api
	_, err = storage.Reload(t.Context(), map[string]string{configreload.AdminKey: "A"})
	require.NoError(t, err)
	_, found := storage.LoadByKey(t.Context(), "B")
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
	storage, api := newSecretStorageForTestWithKey(t, "A", nil)
	_, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: "A"})
	require.NoError(t, err)
	stale := &staleSecretReadClient{Client: api, secret: getSecretForTest(t, api)}
	storage.Client, storage.Cache = stale, newStubCache()
	storage.Run()
	t.Cleanup(storage.Stop)
	finished := make(chan error, 1)
	go func() {
		_, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: "B"})
		finished <- err
	}()
	require.Eventually(t, func() bool { return adminKeyStateSatisfied(getSecretForTest(t, api), "B") }, time.Second, time.Millisecond)
	select {
	case err := <-finished:
		t.Fatalf("accepted before cache observation: %v", err)
	default:
	}
	_, found := storage.LoadByKey(t.Context(), "A")
	require.True(t, found)
	stale.ClearSecret()
	require.NoError(t, <-finished)
	_, found = storage.LoadByKey(t.Context(), "B")
	require.True(t, found)
	_, found = storage.LoadByKey(t.Context(), "A")
	require.False(t, found)
}

func TestMySQLReloadCandidateFailurePreservesRuntime(t *testing.T) {
	st, _, done := newMockStorage(t)
	defer done()
	st.cfg.DSN, st.initialized = reloadTestDSN, true
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
	storage.cfg.DSN = reloadTestDSN
	oldOpen, oldMigrate := openMySQLDB, autoMigrateMySQLModels
	opened, migrated := 0, 0
	openMySQLDB = func(context.Context, string) (*gorm.DB, error) { opened++; return storage.db, nil }
	autoMigrateMySQLModels = func(context.Context, *gorm.DB) error { migrated++; return nil }
	t.Cleanup(func() { openMySQLDB, autoMigrateMySQLModels = oldOpen, oldMigrate })
	expectAdminReload(mock, storage.hashKey("admin"))
	result, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: "admin", configreload.DSN: reloadTestDSN, configreload.Pepper: "pepper"})
	require.NoError(t, err)
	require.Equal(t, configreload.Accepted, result)
	_, err = storage.Reload(t.Context(), map[string]string{configreload.DSN: reloadTestDSN})
	require.NoError(t, err)
	require.Equal(t, 1, opened)
	require.Equal(t, 1, migrated)
}

func TestSecretReloadAdminLength(t *testing.T) {
	for _, tt := range []struct {
		key       string
		minLength int
		want      configreload.Result
	}{
		{"", 12, configreload.Rejected},
		{"短多字节凭据", 12, configreload.Rejected},
		{"twelve-chars", 12, configreload.Accepted},
		{"", 0, configreload.Accepted},
	} {
		t.Run(fmt.Sprintf("%s/min=%d", tt.key, tt.minLength), func(t *testing.T) {
			raw, err := marshalStoredAPIKey(&models.CreatedTeamAPIKey{ID: AdminKeyID, Key: "persisted-admin", Name: "admin", Team: models.AdminTeam()})
			require.NoError(t, err)
			storage, api := newSecretStorageForTestWithKey(t, "", map[string][]byte{AdminKeyID.String(): raw})
			storage.adminKeyMinLength = tt.minLength
			result, err := storage.Reload(t.Context(), map[string]string{configreload.AdminKey: tt.key})
			require.NoError(t, err)
			require.Equal(t, tt.want, result)
			persisted := "persisted-admin"
			if tt.want == configreload.Accepted {
				require.Equal(t, tt.key, storage.AdminKey)
				if tt.key != "" {
					persisted = tt.key
				}
				_, found := storage.LoadByKey(t.Context(), persisted)
				require.True(t, found)
			}
			require.True(t, adminKeyStateSatisfied(getSecretForTest(t, api), persisted))
		})
	}
}
