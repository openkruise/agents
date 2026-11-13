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

package sandbox_manager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	session "github.com/openkruise/agents/pkg/servingsession"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
)

// ServingSession describes a durable binding within an authorized Serving.
type ServingSession struct{ SessionID, ServingID, SandboxID, State, LastRequestAt string }

// ClosedServingSession acknowledges binding removal without stopping its Sandbox.
type ClosedServingSession struct{ SessionID, ServingID, State string }

// ListServingSessionsOptions selects a page of bindings ordered by Session ID.
type ListServingSessionsOptions struct {
	Limit         *int
	ContinueToken string
}

// ServingSessionPage includes an opaque cursor bound to the current Serving UID.
type ServingSessionPage struct {
	Sessions  []ServingSession
	NextToken string
}
type sessionCursor struct {
	UID   string `json:"uid"`
	After string `json:"after"`
}

func mapSessionBackendError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, session.ErrServingNotFound) {
		return managererrors.WrapError(managererrors.ErrorNotFound, err, "SandboxServing not found")
	}
	return managererrors.WrapError(managererrors.ErrorUnavailable, err, "Session manager is unavailable")
}

func (m *SandboxManager) sessionBackend() session.ServingSessionBackend {
	if m.servingBackend != nil {
		return m.servingBackend
	}
	if provider, ok := m.infra.(session.ServingSessionProvider); ok {
		return provider.GetServingSessionBackend()
	}
	return nil
}

func (m *SandboxManager) GetServing(ctx context.Context, namespace, id string) (session.Serving, error) {
	backend := m.sessionBackend()
	if backend == nil {
		return session.Serving{}, mapSessionBackendError(session.ErrServingUnavailable)
	}
	serving, err := backend.GetServing(ctx, namespace, id)
	return serving, mapSessionBackendError(err)
}

func validateSessionID(id string) error {
	if !sessionbinding.ValidSessionID(id) {
		return managererrors.NewError(managererrors.ErrorBadRequest, "Invalid session ID")
	}
	return nil
}

func (m *SandboxManager) GetServingSession(ctx context.Context, namespace, servingID, sessionID string) (ServingSession, error) {
	if err := validateSessionID(sessionID); err != nil {
		return ServingSession{}, err
	}
	serving, err := m.GetServing(ctx, namespace, servingID)
	if err != nil {
		return ServingSession{}, err
	}
	records, err := m.sessionBackend().ReadServingSessions(ctx, serving)
	if err != nil {
		return ServingSession{}, mapSessionBackendError(err)
	}
	for _, record := range records {
		if record.SessionID == sessionID {
			return sessionValue(servingID, record), nil
		}
	}
	return ServingSession{}, managererrors.NewError(managererrors.ErrorNotFound, "Session not found")
}

func sessionValue(servingID string, record session.ServingSession) ServingSession {
	return ServingSession{SessionID: record.SessionID, ServingID: servingID, SandboxID: record.SandboxID, State: record.State, LastRequestAt: record.LastRequestAt}
}

func (m *SandboxManager) ListServingSessions(ctx context.Context, namespace, servingID string, opts ListServingSessionsOptions) (ServingSessionPage, error) {
	limit := 50
	if opts.Limit != nil {
		limit = *opts.Limit
	}
	if limit < 1 || limit > 100 {
		return ServingSessionPage{}, managererrors.NewError(managererrors.ErrorBadRequest, "Limit must be between 1 and 100")
	}
	serving, err := m.GetServing(ctx, namespace, servingID)
	if err != nil {
		return ServingSessionPage{}, err
	}
	after := ""
	if opts.ContinueToken != "" {
		raw, err := base64.RawURLEncoding.Strict().DecodeString(opts.ContinueToken)
		if err != nil || len(opts.ContinueToken) > 1024 || base64.RawURLEncoding.EncodeToString(raw) != opts.ContinueToken {
			return ServingSessionPage{}, managererrors.WrapError(managererrors.ErrorBadRequest, err, "Invalid session cursor")
		}
		var cursor sessionCursor
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&cursor)
		if err == nil {
			var trailing any
			if decoder.Decode(&trailing) != io.EOF {
				err = fmt.Errorf("trailing cursor data")
			}
		}
		canonical, canonicalErr := json.Marshal(cursor)
		if canonicalErr != nil {
			return ServingSessionPage{}, canonicalErr
		}
		if err != nil || string(canonical) != string(raw) || cursor.UID != serving.UID || !sessionbinding.ValidSessionID(cursor.After) {
			return ServingSessionPage{}, managererrors.WrapError(managererrors.ErrorBadRequest, err, "Invalid or stale session cursor")
		}
		after = cursor.After
	}
	records, err := m.sessionBackend().ReadServingSessions(ctx, serving)
	if err != nil {
		return ServingSessionPage{}, mapSessionBackendError(err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].SessionID < records[j].SessionID })
	page := ServingSessionPage{Sessions: make([]ServingSession, 0, limit)}
	for _, record := range records {
		if record.SessionID <= after {
			continue
		}
		if len(page.Sessions) == limit {
			raw, err := json.Marshal(sessionCursor{UID: serving.UID, After: page.Sessions[len(page.Sessions)-1].SessionID})
			if err != nil {
				return ServingSessionPage{}, err
			}
			page.NextToken = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		page.Sessions = append(page.Sessions, sessionValue(servingID, record))
	}
	return page, nil
}

func (m *SandboxManager) CloseServingSession(ctx context.Context, namespace, servingID, sessionID string) (ClosedServingSession, error) {
	if err := validateSessionID(sessionID); err != nil {
		return ClosedServingSession{}, err
	}
	serving, err := m.GetServing(ctx, namespace, servingID)
	if err != nil {
		return ClosedServingSession{}, err
	}
	if err := m.sessionBackend().CloseServingSession(ctx, serving, sessionID); err != nil {
		return ClosedServingSession{}, mapSessionBackendError(err)
	}
	return ClosedServingSession{SessionID: sessionID, ServingID: servingID, State: "Closed"}, nil
}
