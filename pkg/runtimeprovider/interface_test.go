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

package runtimeprovider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewProvider(t *testing.T) {
	tests := []struct {
		name     string
		kind     Kind
		endpoint string
		wantKind Kind
		wantErr  error
	}{
		{
			name:     "empty kind selects the default (envd)",
			kind:     "",
			endpoint: "10.0.0.5",
			wantKind: KindEnvd,
		},
		{
			name:     "explicit envd",
			kind:     KindEnvd,
			endpoint: "10.0.0.5",
			wantKind: KindEnvd,
		},
		{
			name:     "explicit execd",
			kind:     KindExecd,
			endpoint: "10.0.0.5:44772",
			wantKind: KindExecd,
		},
		{
			name:     "unknown kind is rejected",
			kind:     Kind("bogus"),
			endpoint: "10.0.0.5",
			wantErr:  ErrUnknownKind,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewProvider(tt.kind, tt.endpoint)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKind, p.Kind())
		})
	}
}
