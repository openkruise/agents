/*
Copyright 2025.

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

package core

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

func TestNewInPlaceUpdateHandler(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name string
	}{
		{
			name: "CommonInPlaceUpdateHandler implements InPlaceUpdateHandler interface",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			recorder := record.NewFakeRecorder(10)

			handler := &CommonInPlaceUpdateHandler{
				control:  inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
				recorder: recorder,
			}

			if handler == nil {
				t.Fatal("CommonInPlaceUpdateHandler is nil, want non-nil")
			}

			// Verify the struct implements InPlaceUpdateHandler interface
			var _ InPlaceUpdateHandler = handler

			// Verify GetInPlaceUpdateControl returns non-nil
			ctrl := handler.GetInPlaceUpdateControl()
			if ctrl == nil {
				t.Error("GetInPlaceUpdateControl() returned nil, want non-nil")
			}

			// Verify GetRecorder returns the expected recorder
			rec := handler.GetRecorder()
			if rec == nil {
				t.Error("GetRecorder() returned nil, want non-nil")
			}
		})
	}
}
