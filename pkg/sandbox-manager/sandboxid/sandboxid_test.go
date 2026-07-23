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

package sandboxid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// mockObject is a simple implementation of metav1.Object for testing
type mockObject struct {
	metav1.ObjectMeta
}

func TestLegacy(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		nameStr   string
		want      string
	}{
		{
			name:      "simple legacy formatting",
			namespace: "default",
			nameStr:   "test-sandbox",
			want:      "default--test-sandbox",
		},
		{
			name:      "empty namespace",
			namespace: "",
			nameStr:   "test-sandbox",
			want:      "--test-sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Legacy(tt.namespace, tt.nameStr)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGenerateShortID(t *testing.T) {
	tests := []struct {
		name        string
		uid         types.UID
		want        string
		expectError string
	}{
		{
			name:        "valid uuid",
			uid:         types.UID("f47ac10b-58cc-4372-a567-0e02b2c3d479"),
			want:        "6r5mcc2yzrbxfjlhbyblfq6upe",
			expectError: "",
		},
		{
			name:        "invalid uuid",
			uid:         types.UID("invalid-uuid"),
			want:        "",
			expectError: "failed to parse UID as UUID",
		},
		{
			name:        "empty uid",
			uid:         types.UID(""),
			want:        "",
			expectError: "failed to parse UID as UUID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GenerateShortID(tt.uid)
			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Empty(t, got)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name    string
		sandbox metav1.Object
		want    string
	}{
		{
			name:    "nil sandbox",
			sandbox: nil,
			want:    "",
		},
		{
			name: "no label fallback to legacy",
			sandbox: &mockObject{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns",
					Name:      "sbx",
				},
			},
			want: "ns--sbx",
		},
		{
			name: "empty label fallback to legacy",
			sandbox: &mockObject{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns",
					Name:      "sbx",
					Labels: map[string]string{
						LabelKey: "",
					},
				},
			},
			want: "ns--sbx",
		},
		{
			name: "non-empty label resolved directly",
			sandbox: &mockObject{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns",
					Name:      "sbx",
					Labels: map[string]string{
						LabelKey: "6r5mcc2yzrbxfjlhbyblfq6upe",
					},
				},
			},
			want: "6r5mcc2yzrbxfjlhbyblfq6upe",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.sandbox)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAssignShortID(t *testing.T) {
	tests := []struct {
		name        string
		sandbox     metav1.Object
		wantChanged bool
		expectLabel string
		expectError string
	}{
		{
			name:        "nil sandbox returns error",
			sandbox:     nil,
			wantChanged: false,
			expectLabel: "",
			expectError: "sandbox is nil",
		},
		{
			name: "new assignment on unlabeled sandbox",
			sandbox: &mockObject{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("f47ac10b-58cc-4372-a567-0e02b2c3d479"),
				},
			},
			wantChanged: true,
			expectLabel: "6r5mcc2yzrbxfjlhbyblfq6upe",
			expectError: "",
		},
		{
			name: "preserves existing label",
			sandbox: &mockObject{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("f47ac10b-58cc-4372-a567-0e02b2c3d479"),
					Labels: map[string]string{
						LabelKey: "existing-id",
					},
				},
			},
			wantChanged: false,
			expectLabel: "existing-id",
			expectError: "",
		},
		{
			name: "invalid uid during assignment returns error",
			sandbox: &mockObject{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("invalid-uid"),
				},
			},
			wantChanged: false,
			expectLabel: "",
			expectError: "failed to parse UID as UUID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed, err := AssignShortID(tt.sandbox)
			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.False(t, changed)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantChanged, changed)
				if tt.sandbox != nil {
					assert.Equal(t, tt.expectLabel, tt.sandbox.GetLabels()[LabelKey])
				}
			}
		})
	}
}
