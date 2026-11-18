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
	// ServingIDIndex is the local informer field index for the Serving identity label.
	ServingIDIndex       = "servingID"
	ServingIDLabel       = "agents.kruise.io/serving-id"
	ServingNameLabel     = "agents.kruise.io/serving-name"
	ServingUIDLabel      = "agents.kruise.io/serving-uid"
	ServingRevisionLabel = "agents.kruise.io/serving-revision"
	BindingsAnnotation   = "agents.kruise.io/session-bindings"
	MaxRecords           = 512
	MaxBindingBytes      = 128 * 1024
	MaxAnnotationBytes   = 256 * 1024
	Active               = "Active"
)

var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Record contains the persisted state and activity of a Session binding.
type Record struct {
	State         string `json:"state"`
	LastRequestAt string `json:"lastRequestAt"`
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
		if err := json.Unmarshal(fields["state"], &record.State); err != nil {
			return nil, fmt.Errorf("binding %q state: %w", id, err)
		}
		if err := json.Unmarshal(fields["lastRequestAt"], &record.LastRequestAt); err != nil {
			return nil, fmt.Errorf("binding %q lastRequestAt: %w", id, err)
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
	if record.State != Active {
		return fmt.Errorf("binding %q has unsupported state %q", id, record.State)
	}
	timestamp, err := time.Parse(time.RFC3339, record.LastRequestAt)
	if err != nil || len(record.LastRequestAt) != 20 || timestamp.UTC().Format(time.RFC3339) != record.LastRequestAt {
		return fmt.Errorf("binding %q requires an RFC3339 second-resolution UTC lastRequestAt", id)
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
