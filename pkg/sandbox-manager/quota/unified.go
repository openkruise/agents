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

package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPBackend calls the Tobox private account-quota API. It is deliberately a
// SubjectBackend so Redis/API-key quota remains independently compatible.
type HTTPBackend struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewUnifiedBackend(baseURL, token string, timeout time.Duration) (*HTTPBackend, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || token == "" {
		return nil, errors.New("unified quota URL and token are required")
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &HTTPBackend{baseURL: baseURL, token: token, client: &http.Client{Timeout: timeout}}, nil
}

type unifiedQuotaPayload struct {
	ReservationID  string `json:"reservationId"`
	OrganizationID string `json:"organizationId"`
	OwnerID        string `json:"ownerId"`
	SandboxType    string `json:"sandboxType"`
	TemplateID     string `json:"templateId,omitempty"`
	Proof          string `json:"proof,omitempty"`
}

func (b *HTTPBackend) AcquireSubject(ctx context.Context, subject Subject, lockString string) error {
	return b.post(ctx, "/internal/v1/account-quota/hold", unifiedQuotaPayload{
		ReservationID: lockString, OrganizationID: subject.OrganizationID,
		OwnerID: subject.OwnerID, SandboxType: subject.SandboxType,
		TemplateID: subject.TemplateID,
	})
}

func (b *HTTPBackend) ReleaseSubject(ctx context.Context, subject Subject, lockString string) error {
	return b.post(ctx, "/internal/v1/account-quota/release", unifiedQuotaPayload{
		ReservationID: lockString, OrganizationID: subject.OrganizationID,
		OwnerID: subject.OwnerID, SandboxType: subject.SandboxType,
		Proof: "native-gateway-release:" + lockString,
	})
}

func (b *HTTPBackend) UnknownSubject(ctx context.Context, subject Subject, lockString string) error {
	return b.post(ctx, "/internal/v1/account-quota/unknown", unifiedQuotaPayload{
		ReservationID: lockString, OrganizationID: subject.OrganizationID,
		OwnerID: subject.OwnerID, SandboxType: subject.SandboxType,
	})
}

func (b *HTTPBackend) post(ctx context.Context, path string, payload unifiedQuotaPayload) error {
	if b == nil || b.client == nil {
		return errors.New("unified quota backend is not configured")
	}
	if payload.ReservationID == "" || payload.OrganizationID == "" || payload.OwnerID == "" || payload.SandboxType == "" {
		return errors.New("unified quota subject is incomplete")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return ErrQuotaExceeded
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var message struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &message)
		if message.Error == "" {
			message.Error = strings.TrimSpace(string(data))
		}
		if message.Error == "" {
			message.Error = resp.Status
		}
		return fmt.Errorf("unified quota request failed: %s", message.Error)
	}
	return nil
}

// Backend compatibility methods are intentionally inert. The manager invokes
// SubjectBackend methods for this provider; these methods keep the backend
// usable anywhere a Backend is expected without creating a second accounting
// path.
func (b *HTTPBackend) Acquire(context.Context, AcquireParams) error  { return nil }
func (b *HTTPBackend) Release(context.Context, string, string) error { return nil }
func (b *HTTPBackend) ListEntries(context.Context, string) (map[string]Entry, error) {
	return map[string]Entry{}, nil
}
func (b *HTTPBackend) Cleanup(context.Context, string) error { return nil }

var _ Backend = (*HTTPBackend)(nil)
var _ SubjectBackend = (*HTTPBackend)(nil)
