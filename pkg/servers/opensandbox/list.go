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
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/web"
	annotationutils "github.com/openkruise/agents/pkg/utils/annotations"
	"github.com/openkruise/agents/pkg/utils/pagination"
)

// Pagination bounds for `GET /v1/sandboxes`. The OpenSandbox spec fixes the
// defaults (page=1, pageSize=20) and a minimum of 1 but declares no maximum;
// maxPageSize caps a single page so a hostile pageSize cannot force the manager
// to project an unbounded result set. It matches the E2B layer's MaxListLimit
// so both protocols share one operator-visible ceiling.
const (
	defaultListPage     = 1
	defaultListPageSize = 20
	minListPageSize     = 1
	maxListPageSize     = 100
)

// validListStates is the OpenSandbox SandboxState vocabulary accepted by the
// `state` filter. Filtering happens on the projected OpenSandbox state (not the
// agents state), so a client filters in the same vocabulary it reads back.
var validListStates = []SandboxState{
	SandboxStatePending,
	SandboxStateRunning,
	SandboxStatePausing,
	SandboxStatePaused,
	SandboxStateResuming,
	SandboxStateStopping,
	SandboxStateTerminated,
	SandboxStateFailed,
}

// ListSandboxes handles `GET /v1/sandboxes`. Results are scoped to the
// authenticated caller inside Manager.ListSandboxes (namespace + user), so no
// per-sandbox owner middleware is needed. Filtering and pagination follow the
// OpenSandbox spec: `state` (repeatable, OR logic), `metadata` (url-encoded
// k=v&k=v), `page`, `pageSize`.
//
// The agents paginator is cursor-based while OpenSandbox is offset-based, so
// the paginator is run with Limit=0 (all matching sandboxes, sorted by claim
// time) and the page slice is computed here. Per-user sandbox counts are
// quota-bounded, so materializing the filtered set is acceptable for Phase 2.
func (s *Server) ListSandboxes(r *http.Request) (web.ApiResponse[ListSandboxesResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[ListSandboxesResponse]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: errMessageUserNotInContext,
		}
	}

	query, apiErr := parseListQuery(r.URL.Query())
	if apiErr != nil {
		return web.ApiResponse[ListSandboxesResponse]{}, apiErr
	}

	sandboxes, _, err := s.manager.ListSandboxes(ctx, infra.SelectSandboxesOptions{
		Namespace: NamespaceOfUser(user),
		User:      user.ID.String(),
	}, &pagination.Paginator[infra.Sandbox]{
		// Limit=0 returns every sandbox that passes the filter, sorted by the
		// claim-time key; offset pagination is applied below.
		Limit:  0,
		Filter: query.filter,
		GetKey: func(sbx infra.Sandbox) string {
			return sbx.GetAnnotations()[agentsv1alpha1.AnnotationClaimTime]
		},
		GetUniqueKey: func(sbx infra.Sandbox) string {
			return sbx.GetSandboxID()
		},
	})
	if err != nil {
		log.Error(err, "failed to list sandboxes")
		return web.ApiResponse[ListSandboxesResponse]{}, mapLifecycleErrorToApiError(err)
	}

	items, apiErr := projectListPage(sandboxes, query.page, query.pageSize)
	if apiErr != nil {
		return web.ApiResponse[ListSandboxesResponse]{}, apiErr
	}
	return web.ApiResponse[ListSandboxesResponse]{Body: *items}, nil
}

// listQuery is the parsed and validated form of the list query parameters.
type listQuery struct {
	states   []SandboxState
	metadata map[string]string
	page     int
	pageSize int
	filter   func(infra.Sandbox) bool
}

// parseListQuery decodes the OpenSandbox list query parameters. Unknown states,
// malformed pagination, blacklisted metadata keys, and malformed metadata
// encodings are rejected with 400 so a bad filter never silently widens or
// narrows the result set.
func parseListQuery(values url.Values) (listQuery, *web.ApiError) {
	query := listQuery{
		metadata: map[string]string{},
		page:     defaultListPage,
		pageSize: defaultListPageSize,
	}

	for _, raw := range values["state"] {
		// Accept both repeated params (?state=A&state=B) and a comma-separated
		// single param (?state=A,B); the spec uses the repeated form.
		for _, state := range strings.Split(raw, ",") {
			state = strings.TrimSpace(state)
			if state == "" {
				continue
			}
			typed := SandboxState(state)
			if !slices.Contains(validListStates, typed) {
				return query, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("invalid state filter %q", state),
				}
			}
			query.states = append(query.states, typed)
		}
	}

	if encoded := values.Get("metadata"); encoded != "" {
		decoded, err := url.QueryUnescape(encoded)
		if err != nil {
			return query, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("invalid metadata format: %v", err),
			}
		}
		for _, pair := range strings.Split(decoded, "&") {
			if pair == "" {
				continue
			}
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) != 2 {
				return query, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("invalid metadata pair %q: expected key=value", pair),
				}
			}
			if annotationutils.IsBlackListed(kv[0]) {
				return query, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("forbidden metadata key: %s", kv[0]),
				}
			}
			query.metadata[kv[0]] = kv[1]
		}
	}

	if raw := values.Get("page"); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < defaultListPage {
			return query, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("invalid page %q: must be an integer >= %d", raw, defaultListPage),
			}
		}
		query.page = page
	}
	if raw := values.Get("pageSize"); raw != "" {
		pageSize, err := strconv.Atoi(raw)
		if err != nil || pageSize < minListPageSize || pageSize > maxListPageSize {
			return query, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("invalid pageSize %q: must be an integer between %d and %d", raw, minListPageSize, maxListPageSize),
			}
		}
		query.pageSize = pageSize
	}

	query.filter = buildListFilter(query.states, query.metadata)
	return query, nil
}

// buildListFilter returns the paginator filter: a sandbox must be viewable
// (creating and terminally-dead sandboxes are hidden, consistent with
// describe), match every requested state (OR within states, projected into the
// OpenSandbox vocabulary), and carry every requested metadata pair.
func buildListFilter(states []SandboxState, metadata map[string]string) func(infra.Sandbox) bool {
	return func(sbx infra.Sandbox) bool {
		if !isSandboxViewable(sbx) {
			return false
		}
		if len(states) > 0 {
			projected, err := MapSandboxState(sbx)
			if err != nil || !slices.Contains(states, projected) {
				return false
			}
		}
		annotations := sbx.GetAnnotations()
		for k, v := range metadata {
			if annotations[k] != v {
				return false
			}
		}
		return true
	}
}

// projectListPage slices the filtered, sorted sandboxes into the requested page
// and projects each item into the OpenSandbox response shape, filling in the
// required PaginationInfo. Items is always non-nil so an empty page serializes
// as [] rather than null.
func projectListPage(sandboxes []infra.Sandbox, page, pageSize int) (*ListSandboxesResponse, *web.ApiError) {
	totalItems := len(sandboxes)
	totalPages := (totalItems + pageSize - 1) / pageSize

	start := (page - 1) * pageSize
	if start > totalItems {
		start = totalItems
	}
	end := start + pageSize
	if end > totalItems {
		end = totalItems
	}

	items := make([]SandboxResponse, 0, end-start)
	for _, sbx := range sandboxes[start:end] {
		projected, err := projectSandbox(sbx)
		if err != nil {
			// Unreachable for a closed agents state vocabulary; surface as 500
			// rather than silently dropping the item and skewing pagination.
			return nil, &web.ApiError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		items = append(items, projected)
	}

	return &ListSandboxesResponse{
		Items: items,
		Pagination: PaginationInfo{
			Page:        page,
			PageSize:    pageSize,
			TotalItems:  totalItems,
			TotalPages:  totalPages,
			HasNextPage: end < totalItems,
		},
	}, nil
}
