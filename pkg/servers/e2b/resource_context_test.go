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

package e2b

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/servers/web"
)

func TestWithSandboxResource(t *testing.T) {
	tests := []struct {
		name        string
		apiErr      *web.ApiError
		sandbox     metav1.Object
		expectNil   bool
		expectError string
	}{
		{name: "nil error remains nil", sandbox: &metav1.ObjectMeta{}, expectNil: true},
		{name: "nil sandbox does not disclose context", apiErr: &web.ApiError{Message: "failed"}, expectError: "failed"},
		{name: "authorized resource is appended", apiErr: &web.ApiError{Code: 500, Message: "failed"}, sandbox: &metav1.ObjectMeta{Namespace: "team-a", Name: "sandbox-a"}, expectError: "failed; sandboxResource=team-a/sandbox-a"},
		{name: "existing context is not duplicated", apiErr: &web.ApiError{Message: "failed; sandboxResource=team-a/sandbox-a"}, sandbox: &metav1.ObjectMeta{Namespace: "team-a", Name: "sandbox-a"}, expectError: "failed; sandboxResource=team-a/sandbox-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withSandboxResource(tt.apiErr, tt.sandbox)
			if tt.expectNil {
				assert.Nil(t, got)
				return
			}
			assert.Equal(t, tt.expectError, got.Message)
		})
	}
}
