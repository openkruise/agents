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
	session "github.com/openkruise/agents/pkg/sessionservice"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
)

// SessionServiceSession describes a durable binding within an authorized SessionService.
type SessionServiceSession struct{ SessionID, SessionServiceID, SandboxID, LastRequestAt string }

// ClosedSessionServiceSession acknowledges binding removal without stopping its Sandbox.
type ClosedSessionServiceSession struct{ SessionID, SessionServiceID string }

// ListSessionServiceSessionsOptions selects a page of bindings ordered by Session ID.
type ListSessionServiceSessionsOptions struct {
	Limit         *int
	ContinueToken string
}

// SessionServiceSessionPage includes an opaque cursor bound to the current SessionService UID.
type SessionServiceSessionPage struct {
	Sessions  []SessionServiceSession
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
	if errors.Is(err, session.ErrSessionServiceNotFound) {
		return managererrors.WrapError(managererrors.ErrorNotFound, err, "SessionService not found")
	}
	if errors.Is(err, session.ErrSessionNotFound) {
		return managererrors.WrapError(managererrors.ErrorNotFound, err, "Session not found")
	}
	return managererrors.WrapError(managererrors.ErrorUnavailable, err, "Session manager is unavailable")
}

func (m *SandboxManager) sessionBackend() session.SessionServiceSessionBackend {
	if m.servingBackend != nil {
		return m.servingBackend
	}
	if provider, ok := m.infra.(session.SessionServiceSessionProvider); ok {
		return provider.GetSessionServiceSessionBackend()
	}
	return nil
}

func (m *SandboxManager) GetSessionService(ctx context.Context, namespace, id string) (session.SessionService, error) {
	backend := m.sessionBackend()
	if backend == nil {
		return session.SessionService{}, mapSessionBackendError(session.ErrSessionServiceUnavailable)
	}
	serving, err := backend.GetSessionService(ctx, namespace, id)
	return serving, mapSessionBackendError(err)
}

func validateSessionID(id string) error {
	if !sessionbinding.ValidSessionID(id) {
		return managererrors.NewError(managererrors.ErrorBadRequest, "Invalid session ID")
	}
	return nil
}

func (m *SandboxManager) GetSessionServiceSession(ctx context.Context, namespace, sessionServiceID, sessionID string) (SessionServiceSession, error) {
	if err := validateSessionID(sessionID); err != nil {
		return SessionServiceSession{}, err
	}
	serving, err := m.GetSessionService(ctx, namespace, sessionServiceID)
	if err != nil {
		return SessionServiceSession{}, err
	}
	records, err := m.sessionBackend().ReadSessionServiceSessions(ctx, serving)
	if err != nil {
		return SessionServiceSession{}, mapSessionBackendError(err)
	}
	for _, record := range records {
		if record.SessionID == sessionID {
			return sessionValue(sessionServiceID, record), nil
		}
	}
	return SessionServiceSession{}, managererrors.NewError(managererrors.ErrorNotFound, "Session not found")
}

func sessionValue(sessionServiceID string, record session.SessionServiceSession) SessionServiceSession {
	return SessionServiceSession{SessionID: record.SessionID, SessionServiceID: sessionServiceID, SandboxID: record.SandboxID, LastRequestAt: record.LastRequestAt}
}

func (m *SandboxManager) ListSessionServiceSessions(ctx context.Context, namespace, sessionServiceID string, opts ListSessionServiceSessionsOptions) (SessionServiceSessionPage, error) {
	limit := 50
	if opts.Limit != nil {
		limit = *opts.Limit
	}
	if limit < 1 || limit > 100 {
		return SessionServiceSessionPage{}, managererrors.NewError(managererrors.ErrorBadRequest, "Limit must be between 1 and 100")
	}
	serving, err := m.GetSessionService(ctx, namespace, sessionServiceID)
	if err != nil {
		return SessionServiceSessionPage{}, err
	}
	after := ""
	if opts.ContinueToken != "" {
		raw, err := base64.RawURLEncoding.Strict().DecodeString(opts.ContinueToken)
		if err != nil || len(opts.ContinueToken) > 1024 || base64.RawURLEncoding.EncodeToString(raw) != opts.ContinueToken {
			return SessionServiceSessionPage{}, managererrors.WrapError(managererrors.ErrorBadRequest, err, "Invalid session cursor")
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
			return SessionServiceSessionPage{}, canonicalErr
		}
		if err != nil || string(canonical) != string(raw) || cursor.UID != serving.UID || !sessionbinding.ValidSessionID(cursor.After) {
			return SessionServiceSessionPage{}, managererrors.WrapError(managererrors.ErrorBadRequest, err, "Invalid or stale session cursor")
		}
		after = cursor.After
	}
	records, err := m.sessionBackend().ReadSessionServiceSessions(ctx, serving)
	if err != nil {
		return SessionServiceSessionPage{}, mapSessionBackendError(err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].SessionID < records[j].SessionID })
	page := SessionServiceSessionPage{Sessions: make([]SessionServiceSession, 0, limit)}
	for _, record := range records {
		if record.SessionID <= after {
			continue
		}
		if len(page.Sessions) == limit {
			raw, err := json.Marshal(sessionCursor{UID: serving.UID, After: page.Sessions[len(page.Sessions)-1].SessionID})
			if err != nil {
				return SessionServiceSessionPage{}, err
			}
			page.NextToken = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		page.Sessions = append(page.Sessions, sessionValue(sessionServiceID, record))
	}
	return page, nil
}

func (m *SandboxManager) CloseSessionServiceSession(ctx context.Context, namespace, sessionServiceID, sessionID string) (ClosedSessionServiceSession, error) {
	if err := validateSessionID(sessionID); err != nil {
		return ClosedSessionServiceSession{}, err
	}
	serving, err := m.GetSessionService(ctx, namespace, sessionServiceID)
	if err != nil {
		return ClosedSessionServiceSession{}, err
	}
	if err := m.sessionBackend().CloseSessionServiceSession(ctx, serving, sessionID); err != nil {
		return ClosedSessionServiceSession{}, mapSessionBackendError(err)
	}
	if m.servingAllocator != nil {
		m.servingAllocator.ForgetSession(serving.UID, sessionID)
	}
	return ClosedSessionServiceSession{SessionID: sessionID, SessionServiceID: sessionServiceID}, nil
}
