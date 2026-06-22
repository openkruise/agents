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

package config

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDefaultAccessToken(t *testing.T) {
	token := NewDefaultAccessToken()
	assert.NotEmpty(t, token, "token should not be empty")
	_, err := uuid.Parse(token)
	require.NoError(t, err, "token should be a valid UUID")

	// Consecutive calls should produce unique tokens
	token2 := NewDefaultAccessToken()
	assert.NotEqual(t, token, token2, "consecutive calls should produce unique tokens")
}

func TestDefaultCSIMountConcurrency(t *testing.T) {
	assert.Equal(t, 3, DefaultCSIMountConcurrency, "default CSI mount concurrency should be 3")
}
