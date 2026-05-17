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

package tokeninjection

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func ptr[T any](v T) *T {
	return &v
}

func TestCheckWhenCondition_NilCondition(t *testing.T) {
	headers := map[string]string{"authorization": "Bearer anything"}
	ok, err := CheckWhenCondition(nil, headers)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("nil condition should always return true")
	}
}

func TestCheckWhenCondition_HeaderMissing(t *testing.T) {
	when := &v1alpha1.ActionCondition{Header: "Authorization", Pattern: "^Bearer __PLACEHOLDER__"}
	headers := map[string]string{"other-header": "value"}

	ok, err := CheckWhenCondition(when, headers)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if ok {
		t.Error("should return false when header is missing")
	}
}

func TestCheckWhenCondition_PatternMatch(t *testing.T) {
	when := &v1alpha1.ActionCondition{Header: "Authorization", Pattern: "^Bearer __PLACEHOLDER__"}

	tests := []struct {
		name  string
		value string
		match bool
	}{
		{"simple bearer token", "Bearer eyJhbGciOiJIUzI1NiJ9.abc123", true},
		{"Bearer with prefix", "Bearer some-token-value", true},
		{"wrong prefix", "Basic some-credentials", false},
		{"empty header value", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{"authorization": tt.value}
			ok, err := CheckWhenCondition(when, headers)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if ok != tt.match {
				t.Errorf("expected match=%v, got %v for value %q", tt.match, ok, tt.value)
			}
		})
	}
}

// TestCheckWhenCondition_BothAnchorsNoPlaceholder pins the fix for a previously
// dead branch in escapeAndBuildPattern: when a pattern contains BOTH ^ and $
// anchors and NO __PLACEHOLDER__, the trailing $ used to be regex-escaped and
// thus matched a literal "$" rather than acting as an end-of-string anchor.
func TestCheckWhenCondition_BothAnchorsNoPlaceholder(t *testing.T) {
	when := &v1alpha1.ActionCondition{Header: "X-Tag", Pattern: "^literal$"}

	tests := []struct {
		name  string
		value string
		match bool
	}{
		{"exact value matches", "literal", true},
		{"trailing junk does NOT match (true end anchor)", "literal-and-more", false},
		{"trailing literal $ does NOT match", "literal$", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{"x-tag": tt.value}
			ok, err := CheckWhenCondition(when, headers)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if ok != tt.match {
				t.Errorf("expected match=%v, got %v for value %q", tt.match, ok, tt.value)
			}
		})
	}
}

func TestCheckWhenCondition_ComplexPattern(t *testing.T) {
	when := &v1alpha1.ActionCondition{
		Header:  "Proxy-Authorization",
		Pattern: "Token __PLACEHOLDER__ v2",
	}

	tests := []struct {
		name  string
		value string
		match bool
	}{
		{"match pattern", "Token abc123 v2", true},
		{"missing v2 suffix", "Token abc123", false},
		{"missing Token prefix", "abc123 v2", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{"proxy-authorization": tt.value}
			ok, err := CheckWhenCondition(when, headers)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if ok != tt.match {
				t.Errorf("expected match=%v, got %v", tt.match, ok)
			}
		})
	}
}

func TestBuildHeaderValue_NoTemplate(t *testing.T) {
	result, err := BuildHeaderValue("Bearer {{ .Token }}", "my-token")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if result != "Bearer my-token" {
		t.Errorf("expected 'Bearer my-token', got %q", result)
	}
}

func TestBuildHeaderValue_WithTemplate(t *testing.T) {
	result, err := BuildHeaderValue("{{ .Token }}", "my-token")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if result != "my-token" {
		t.Errorf("expected 'my-token', got %q", result)
	}
}

func TestBuildHeaderValue_ComplexTemplate(t *testing.T) {
	result, err := BuildHeaderValue("Bearer {{ .Token }}; version=2", "secret123")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if result != "Bearer secret123; version=2" {
		t.Errorf("expected 'Bearer secret123; version=2', got %q", result)
	}
}

func TestBuildHeaderValue_InvalidTemplate(t *testing.T) {
	_, err := BuildHeaderValue("{{ .Token", "token")
	if err == nil {
		t.Error("expected error for invalid template syntax")
	}
}

func TestValidateTokenProviderRef_Valid(t *testing.T) {
	ref := &corev1.TypedLocalObjectReference{
		APIGroup: ptr("agentidentity.alibabacloud.com"),
		Kind:     "CredentialProvider",
		Name:     "llm-api-key",
	}
	err := ValidateTokenProviderRef(ref)
	if err != nil {
		t.Errorf("expected valid reference, got error: %v", err)
	}
}

func TestValidateTokenProviderRef_NilRef(t *testing.T) {
	err := ValidateTokenProviderRef(nil)
	if err == nil {
		t.Error("expected error for nil ref")
	}
}

func TestValidateTokenProviderRef_InvalidKind(t *testing.T) {
	ref := &corev1.TypedLocalObjectReference{
		APIGroup: ptr("agentidentity.alibabacloud.com"),
		Kind:     "Secret",
		Name:     "my-secret",
	}
	err := ValidateTokenProviderRef(ref)
	if err == nil {
		t.Error("expected error for invalid kind")
	}
}

func TestValidateTokenProviderRef_InvalidGroup(t *testing.T) {
	ref := &corev1.TypedLocalObjectReference{
		APIGroup: ptr("other.group.com"),
		Kind:     "CredentialProvider",
		Name:     "my-provider",
	}
	err := ValidateTokenProviderRef(ref)
	if err == nil {
		t.Error("expected error for invalid group")
	}
}

func TestValidateTokenProviderRef_EmptyName(t *testing.T) {
	ref := &corev1.TypedLocalObjectReference{
		APIGroup: ptr("agentidentity.alibabacloud.com"),
		Kind:     "CredentialProvider",
		Name:     "",
	}
	err := ValidateTokenProviderRef(ref)
	if err == nil {
		t.Error("expected error for empty name")
	}
}
