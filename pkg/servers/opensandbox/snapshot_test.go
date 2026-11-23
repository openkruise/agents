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
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

const snapshotTestCreatedAt = "2026-11-15T10:00:00Z"

type snapshotCreateTestInfra struct {
	createIdentityInfra
	records []infra.CheckpointInfo
	readErr error
	list    []infra.SelectSucceededCheckpointsOptions
	deletes int
}

func (b *snapshotCreateTestInfra) Build() infra.Infrastructure {
	return b
}

func (b *snapshotCreateTestInfra) SelectSucceededCheckpoints(_ context.Context, opts infra.SelectSucceededCheckpointsOptions) ([]infra.CheckpointInfo, error) {
	b.list = append(b.list, opts)
	return b.records, b.readErr
}

func (b *snapshotCreateTestInfra) DeleteCheckpoint(context.Context, infra.DeleteCheckpointOptions) error {
	b.deletes++
	return errors.New("unexpected checkpoint delete")
}

type snapshotCreateTestSource struct {
	infra.Sandbox
	create func(infra.CreateCheckpointOptions) (string, error)
}

func (s snapshotCreateTestSource) CreateCheckpoint(_ context.Context, opts infra.CreateCheckpointOptions) (string, error) {
	return s.create(opts)
}

func TestValidateCreateSnapshotBody(t *testing.T) {
	for _, body := range []string{"", `{}`, `null`, `{"name":null}`, `{"name":""}`, `{"name":" "}`} {
		assert.Nil(t, validateCreateSnapshotBody(strings.NewReader(body)), "%q", body)
	}
	for _, body := range []string{`[]`, `{"name":`, `{} null`} {
		apiErr := validateCreateSnapshotBody(strings.NewReader(body))
		require.NotNil(t, apiErr, "%q", body)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code, "%q", body)
	}
}

func TestCreateSnapshotHTTP(t *testing.T) {
	const (
		namespace = "team-a"
		sourceID  = "team-a--source"
		artifact  = "checkpoint-id"
	)
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: &models.Team{Name: namespace}}
	store := &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{"own-key": user}}

	for _, tt := range []struct {
		name       string
		body       string
		missing    bool
		mutate     func(*agentsv1alpha1.Sandbox)
		nativeErr  error
		readErr    error
		projectErr bool
		status     int
		creates    int
		reads      int
	}{
		{name: "name and unknown fields ignored", body: `{"name":"example","unknown":true}`, status: http.StatusAccepted, creates: 1, reads: 1},
		{name: "wrong name type", body: `{"name":true}`, status: http.StatusBadRequest},
		{name: "missing source", missing: true, status: http.StatusNotFound},
		{name: "foreign source", mutate: func(s *agentsv1alpha1.Sandbox) {
			s.Annotations[agentsv1alpha1.AnnotationOwner] = "foreign-key-id"
		}, status: http.StatusNotFound},
		{name: "reserved failed source", mutate: func(s *agentsv1alpha1.Sandbox) {
			s.Labels = map[string]string{agentsv1alpha1.LabelSandboxReservedFailed: agentsv1alpha1.True}
		}, status: http.StatusNotFound},
		{name: "wrong source state", mutate: func(s *agentsv1alpha1.Sandbox) {
			s.Status.Phase = agentsv1alpha1.SandboxPending
		}, status: http.StatusNotFound},
		{name: "paused source", mutate: func(s *agentsv1alpha1.Sandbox) {
			s.Spec.Paused = true
		}, status: http.StatusBadRequest},
		{name: "native conflict", nativeErr: managererrors.NewError(managererrors.ErrorConflict, "native conflict"), status: http.StatusConflict, creates: 1},
		{name: "readback failure", readErr: errors.New("checkpoint cache unavailable"), status: http.StatusInternalServerError, creates: 1, reads: 1},
		{name: "projection failure", projectErr: true, status: http.StatusInternalServerError, creates: 1, reads: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   namespace,
					Name:        "source",
					Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: user.ID.String()},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					}},
				},
			}
			if tt.mutate != nil {
				tt.mutate(source)
			}
			record := infra.CheckpointInfo{
				Name:              "artifact-cr",
				Namespace:         namespace,
				CheckpointID:      artifact,
				SandboxID:         sourceID,
				Phase:             string(agentsv1alpha1.CheckpointSucceeded),
				CreationTimestamp: snapshotTestCreatedAt,
			}
			if tt.projectErr {
				record.CreationTimestamp = "invalid-time"
			}
			backend := &snapshotCreateTestInfra{records: []infra.CheckpointInfo{record}, readErr: tt.readErr}
			creates := 0
			if !tt.missing {
				backend.result = snapshotCreateTestSource{Sandbox: sandboxcr.AsSandbox(source, nil), create: func(opts infra.CreateCheckpointOptions) (string, error) {
					creates++
					assert.Equal(t, infra.CreateCheckpointOptions{}, opts)
					return artifact, tt.nativeErr
				}}
			}
			mux := newControlPlaneTestMux(t, store, backend)
			r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sourceID+"/snapshots", strings.NewReader(tt.body))
			r.Header.Set("x-e2b-kruise-snapshot-keep-running", "false")
			r.Header.Set("x-e2b-kruise-snapshot-wait-success-seconds", "5")
			r.Header.Set(HeaderOpenSandboxAPIKey, "own-key")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			assert.Equal(t, tt.creates, creates)
			assert.Len(t, backend.list, tt.reads)
			for _, opts := range backend.list {
				assert.Equal(t, infra.SelectSucceededCheckpointsOptions{Namespace: namespace, User: user.ID.String()}, opts)
			}
			assert.Zero(t, backend.deletes)
			switch tt.status {
			case http.StatusAccepted:
				assert.Empty(t, w.Header().Get("Location"))
				assert.JSONEq(t, `{"id":"checkpoint-id","sandboxId":"team-a--source","status":{"state":"Ready"},"createdAt":"2026-11-15T10:00:00Z"}`, w.Body.String())
			case http.StatusNotFound:
				assert.JSONEq(t, `{"code":"NOT_FOUND","message":"sandbox not found"}`, w.Body.String())
			case http.StatusInternalServerError:
				assert.Contains(t, w.Body.String(), artifact)
			}
		})
	}
}

func TestListSnapshotsHTTP(t *testing.T) {
	team := &models.Team{Name: "team-a"}
	owner := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: team}
	otherKey := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: team}
	admin := &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Team: models.AdminTeam()}
	store := &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{"owner": owner, "other": otherKey, "admin": admin}}
	mux, _ := snapshotTestCacheMux(t, store,
		snapshotFixture("team-a", "b-record", "first-source", "b-id", owner),
		snapshotFixture("team-a", "a-record", "first-source", "a-id", owner),
		snapshotFixture("team-a", "c-record", "second-source", "c-id", owner),
		snapshotFixture("team-a", "other-key", "first-source", "other-id", otherKey),
		snapshotFixture("team-b", "cross-ns", "first-source", "cross-ns-id", owner),
		snapshotFixture("team-a", "admin-a", "first-source", "admin-id-a", admin),
		snapshotFixture("team-b", "admin-b", "first-source", "admin-id-b", admin),
		snapshotFixture("team-a", "pending", "first-source", "pending-id", owner, func(cp *agentsv1alpha1.Checkpoint) {
			cp.Status.Phase = agentsv1alpha1.CheckpointPending
		}),
	)
	for _, tt := range []struct {
		name      string
		query     string
		key       string
		status    int
		ids       []string
		page      int
		pageSize  int
		total     int
		totalPage int
		hasNext   bool
	}{
		{name: "default with deterministic same-time ordering", key: "owner", ids: []string{"a-id", "b-id", "c-id"}, page: 1, pageSize: 20, total: 3, totalPage: 1},
		{name: "admin across namespaces retaining owner", key: "admin", ids: []string{"admin-id-a", "admin-id-b"}, page: 1, pageSize: 20, total: 2, totalPage: 1},
		{name: "same team other key", key: "other", ids: []string{"other-id"}, page: 1, pageSize: 20, total: 1, totalPage: 1},
		{name: "first page with OR states AND source", key: "owner", query: "?state=Unknown&state=Ready&sandboxId=first-source&pageSize=1", ids: []string{"a-id"}, page: 1, pageSize: 1, total: 2, totalPage: 2, hasNext: true},
		{name: "next page", key: "owner", query: "?sandboxId=first-source&pageSize=1&page=2", ids: []string{"b-id"}, page: 2, pageSize: 1, total: 2, totalPage: 2},
		{name: "empty out-of-range page keeps totals", key: "owner", query: "?page=" + strconv.Itoa(math.MaxInt), page: math.MaxInt, pageSize: 20, total: 3, totalPage: 1},
		{name: "explicit empty name", key: "owner", query: "?name=", page: 1, pageSize: 20},
		{name: "CR name does not match a human name", key: "owner", query: "?name=a-record", page: 1, pageSize: 20},
		{name: "unknown and comma-separated states", key: "owner", query: "?state=ready&state=Ready,Failed", page: 1, pageSize: 20},
		{name: "null source is literal", key: "owner", query: "?sandboxId=null", page: 1, pageSize: 20},
		{name: "empty source omits filter", key: "other", query: "?sandboxId=&pageSize=100", ids: []string{"other-id"}, page: 1, pageSize: 100, total: 1, totalPage: 1},
		{name: "empty page", key: "owner", query: "?page=", status: http.StatusBadRequest},
		{name: "overflow page", key: "owner", query: "?page=" + strconv.Itoa(math.MaxInt) + "0", status: http.StatusBadRequest},
		{name: "invalid page size", key: "owner", query: "?pageSize=101", status: http.StatusBadRequest},
		{name: "invalid key", key: "bad-key", status: http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := snapshotTestRequest(mux, http.MethodGet, "/v1/snapshots"+tt.query, tt.key)
			if tt.status != 0 {
				assert.Equal(t, tt.status, w.Code, w.Body.String())
				return
			}
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var response ListSnapshotsResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.NotNil(t, response.Items, "empty results must serialize as []")
			var ids []string
			for _, item := range response.Items {
				ids = append(ids, item.ID)
				assert.Equal(t, snapshotStateReady, item.Status.State)
				assert.Equal(t, snapshotTestCreatedAt, item.CreatedAt)
			}
			assert.Equal(t, tt.ids, ids)
			assert.Equal(t, PaginationInfo{
				Page: tt.page, PageSize: tt.pageSize, TotalItems: tt.total, TotalPages: tt.totalPage, HasNextPage: tt.hasNext,
			}, response.Pagination)
		})
	}
}

func TestListSnapshotsHTTPAuthDisabled(t *testing.T) {
	foreign := &models.CreatedTeamAPIKey{ID: uuid.New()}
	mux, _ := snapshotTestCacheMux(t, nil,
		snapshotFixture("team-a", "anonymous-record", "source-id", "anonymous-id", anonymousUser),
		snapshotFixture("team-a", "foreign-record", "source-id", "foreign-id", foreign),
	)
	w := snapshotTestRequest(mux, http.MethodGet, "/v1/snapshots", "ignored-key")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response ListSnapshotsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Len(t, response.Items, 1)
	assert.Equal(t, "anonymous-id", response.Items[0].ID)
}

func TestListSnapshotsHTTPBackendErrors(t *testing.T) {
	backend := &snapshotCreateTestInfra{readErr: errors.New("checkpoint cache unavailable")}
	mux := newControlPlaneTestMux(t, nil, backend)
	w := snapshotTestRequest(mux, http.MethodGet, "/v1/snapshots", "")
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	backend.readErr = nil
	backend.records = []infra.CheckpointInfo{{
		Name: "record", Namespace: "team-a", Phase: string(agentsv1alpha1.CheckpointSucceeded),
		CheckpointID: "missing-source-id", CreationTimestamp: snapshotTestCreatedAt,
	}}
	w = snapshotTestRequest(mux, http.MethodGet, "/v1/snapshots", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
}

func TestListSnapshotsHTTPCreationOrder(t *testing.T) {
	later := infra.CheckpointInfo{
		Name: "later", Namespace: "team-a", SandboxID: "source", CheckpointID: "later",
		Phase: string(agentsv1alpha1.CheckpointSucceeded), CreationTimestamp: snapshotTestCreatedAt,
	}
	earlier := later
	earlier.Name, earlier.CheckpointID = "earlier", "earlier"
	earlier.CreationTimestamp = "2026-11-15T18:00:00+09:00"
	malformed := later
	malformed.Name, malformed.CheckpointID = "malformed", "malformed"
	malformed.CreationTimestamp = ""
	backend := &snapshotCreateTestInfra{records: []infra.CheckpointInfo{later, earlier, malformed}}
	mux := newControlPlaneTestMux(t, nil, backend)

	w := snapshotTestRequest(mux, http.MethodGet, "/v1/snapshots?pageSize=1", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response ListSnapshotsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Len(t, response.Items, 1)
	assert.Equal(t, "earlier", response.Items[0].ID)
	assert.Equal(t, "2026-11-15T09:00:00Z", response.Items[0].CreatedAt)
	assert.Equal(t, 3, response.Pagination.TotalItems)
	w = snapshotTestRequest(mux, http.MethodGet, "/v1/snapshots?page=3&pageSize=1", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
}

func TestDeleteSnapshotHTTP(t *testing.T) {
	team := &models.Team{Name: "team-a"}
	owner := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: team}
	other := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: team}
	store := &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{"owner": owner, "other": other}}
	checkpoint := snapshotFixture(team.Name, "snapshot-cr", "deleted-source", "shared-id", owner)
	template := &agentsv1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: team.Name, Name: checkpoint.Name}}
	set := &agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{Namespace: team.Name, Name: checkpoint.Status.CheckpointId}}
	mux, kube := snapshotTestCacheMux(t, store, checkpoint, template, set)

	path := "/v1/snapshots/shared-id"
	w := snapshotTestRequest(mux, http.MethodDelete, path, "other")
	assert.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.Empty(t, w.Body.String())
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(checkpoint), &agentsv1alpha1.Checkpoint{}))
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(template), &agentsv1alpha1.SandboxTemplate{}))

	w = snapshotTestRequest(mux, http.MethodDelete, path, "owner")
	assert.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.Empty(t, w.Body.String())
	assert.True(t, apierrors.IsNotFound(kube.Get(t.Context(), client.ObjectKeyFromObject(checkpoint), &agentsv1alpha1.Checkpoint{})))
	assert.True(t, apierrors.IsNotFound(kube.Get(t.Context(), client.ObjectKeyFromObject(template), &agentsv1alpha1.SandboxTemplate{})))
	assert.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(set), &agentsv1alpha1.SandboxSet{}))

	w = snapshotTestRequest(mux, http.MethodDelete, path, "owner")
	assert.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
}

func snapshotFixture(namespace, name, sourceID, artifactID string, user *models.CreatedTeamAPIKey, mutators ...func(*agentsv1alpha1.Checkpoint)) *agentsv1alpha1.Checkpoint {
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              name,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)),
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner:     user.ID.String(),
				agentsv1alpha1.AnnotationSandboxID: sourceID,
			},
		},
		Status: agentsv1alpha1.CheckpointStatus{
			CheckpointId: artifactID,
			Phase:        agentsv1alpha1.CheckpointSucceeded,
		},
	}
	for _, mutate := range mutators {
		mutate(cp)
	}
	return cp
}

func snapshotTestCacheMux(t *testing.T, store keys.KeyStorage, objects ...client.Object) (*http.ServeMux, client.Client) {
	t.Helper()
	cache, fc, err := cachetest.NewTestCache(t, objects...)
	require.NoError(t, err)
	opts := config.SandboxManagerOptions{DisableEnvoyExtProc: true}
	return newControlPlaneTestMux(t, store, sandboxcr.NewInfraBuilder(opts).WithCache(cache).WithAPIReader(fc)), fc
}

func newControlPlaneTestMux(t *testing.T, store keys.KeyStorage, backend infra.Builder) *http.ServeMux {
	t.Helper()
	opts := config.SandboxManagerOptions{DisableEnvoyExtProc: true}
	manager, err := sandboxmanager.NewSandboxManagerBuilder(opts).WithCustomInfra(func() (infra.Builder, error) {
		return backend, nil
	}).Build()
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: manager, Keys: store}))
	return mux
}

func snapshotTestRequest(mux *http.ServeMux, method, target, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set(HeaderOpenSandboxAPIKey, key)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}
