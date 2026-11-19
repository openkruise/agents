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

// Package binding defines the protocol-neutral persisted binding format.
package binding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"time"
)

const (
	SessionServiceUIDLabel      = "agents.kruise.io/session-service-uid"
	SessionServiceRevisionLabel = "agents.kruise.io/session-service-revision"
	BindingsAnnotation          = "agents.kruise.io/session-bindings"
	Moving                      = "Moving"
	MaxRecords                  = 512
	MaxBindingBytes             = 128 * 1024
	MaxAnnotationBytes          = 256 * 1024
)

var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// Record stores immutable Session creation time and its durable migration target.
type Record struct {
	CreatedAt string `json:"createdAt"`

	Phase string `json:"phase,omitempty"`
	// TargetSandboxUID persists the chosen destination before any target write.
	// A new leader must resume this UID: choosing again after a lost response or
	// cache gap could bind the same session to two Sandboxes. UID also prevents
	// a same-name replacement from being mistaken for the original destination.
	TargetSandboxUID string `json:"targetSandboxUID,omitempty"`
}

type Bindings map[string]Record

// Decode treats only an absent annotation or a valid empty object as empty.
func Decode(annotations map[string]string) (Bindings, error) {
	if err := checkAnnotationBudget(annotations); err != nil {
		return nil, err
	}
	raw, exists := annotations[BindingsAnnotation]
	if !exists {
		return Bindings{}, nil
	}
	if len(BindingsAnnotation)+len(raw) > MaxBindingBytes {
		return nil, fmt.Errorf("binding annotation exceeds %d bytes", MaxBindingBytes)
	}
	values, err := object([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("decode session bindings: %w", err)
	}
	if len(values) > MaxRecords {
		return nil, fmt.Errorf("session bindings exceed %d records", MaxRecords)
	}
	bindings := make(Bindings, len(values))
	for id, value := range values {
		fields, err := object(value)
		if err != nil {
			return nil, fmt.Errorf("decode binding %q: %w", id, err)
		}
		var record Record
		if err := json.Unmarshal(value, &record); err != nil {
			return nil, fmt.Errorf("binding %q: %w", id, err)
		}
		if err := json.Unmarshal(fields["createdAt"], &record.CreatedAt); err != nil {
			return nil, fmt.Errorf("binding %q createdAt: %w", id, err)
		}
		if err := validate(id, record); err != nil {
			return nil, err
		}
		bindings[id] = record
	}
	return bindings, nil
}

// Encode returns a copy of annotations and enforces serialized key/value budgets.
func Encode(annotations map[string]string, bindings Bindings) (map[string]string, error) {
	if len(bindings) > MaxRecords {
		return nil, fmt.Errorf("session bindings exceed %d records", MaxRecords)
	}
	for id, record := range bindings {
		if err := validate(id, record); err != nil {
			return nil, err
		}
	}
	// An empty binding set is always an object, including a nil input map.
	if bindings == nil {
		bindings = Bindings{}
	}
	raw, err := json.Marshal(bindings)
	if err != nil {
		return nil, fmt.Errorf("encode session bindings: %w", err)
	}
	if len(BindingsAnnotation)+len(raw) > MaxBindingBytes {
		return nil, fmt.Errorf("binding annotation exceeds %d bytes", MaxBindingBytes)
	}
	result := make(map[string]string, len(annotations)+1)
	for key, value := range annotations {
		result[key] = value
	}
	result[BindingsAnnotation] = string(raw)
	if err := checkAnnotationBudget(result); err != nil {
		return nil, err
	}
	return result, nil
}

// ValidSessionID accepts the persisted, case-sensitive ASCII session identity.
func ValidSessionID(id string) bool { return sessionID.MatchString(id) }

func validate(id string, record Record) error {
	if !ValidSessionID(id) {
		return fmt.Errorf("invalid session ID %q", id)
	}
	if record.Phase != "" && record.Phase != Moving {
		return fmt.Errorf("binding %q has unsupported phase %q", id, record.Phase)
	}
	if record.Phase != Moving && record.TargetSandboxUID != "" {
		return fmt.Errorf("binding %q requires Moving for a target", id)
	}
	timestamp, err := time.Parse(time.RFC3339, record.CreatedAt)
	if err != nil || len(record.CreatedAt) != 20 || timestamp.UTC().Format(time.RFC3339) != record.CreatedAt {
		return fmt.Errorf("binding %q requires an RFC3339 second-resolution UTC createdAt", id)
	}
	return nil
}

func checkAnnotationBudget(annotations map[string]string) error {
	total := 0
	for key, value := range annotations {
		total += len(key) + len(value)
	}
	if total > MaxAnnotationBytes {
		return fmt.Errorf("annotations exceed %d bytes", MaxAnnotationBytes)
	}
	return nil
}

// object rejects null, duplicate keys and trailing JSON rather than losing data.
func object(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if start != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object")
	}
	result := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected object key")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		result[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON: %v", err)
	}
	return result, nil
}

// ExpiresAt returns the fixed creation deadline, or zero when expiry is disabled.
// Moving records and their Active targets use the same creation timestamp.
func ExpiresAt(record Record, lifetime string) (time.Time, error) {
	if lifetime == "" {
		return time.Time{}, nil
	}
	duration, err := time.ParseDuration(lifetime)
	if err != nil || duration <= 0 {
		return time.Time{}, fmt.Errorf("invalid Session creation lifetime %q", lifetime)
	}
	created, err := time.Parse(time.RFC3339, record.CreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid Session creation time: %w", err)
	}
	return created.Add(duration), nil
}
