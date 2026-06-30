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

package core

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// stubControl is a minimal CommitControl used in factory tests.
type stubControl struct{}

func (s *stubControl) EnsureCommitRunning(_ context.Context, _ *EnsureFuncArgs) (time.Duration, error) {
	return 0, nil
}
func (s *stubControl) EnsureCommitUpdated(_ context.Context, _ *EnsureFuncArgs) (time.Duration, error) {
	return 0, nil
}
func (s *stubControl) EnsureCommitDeleted(_ context.Context, _ *EnsureFuncArgs) (time.Duration, error) {
	return 0, nil
}

func TestNewCommitControl_Success(t *testing.T) {
	// Save and restore global state
	origFactories := commitControlFactories
	defer func() { commitControlFactories = origFactories }()

	commitControlFactories = []CommitControlFactory{
		{
			Name:     "test-stub",
			Required: true,
			New: func(c client.Client, r record.EventRecorder) (CommitControl, error) {
				return &stubControl{}, nil
			},
		},
	}

	fc := fake.NewClientBuilder().Build()
	recorder := record.NewFakeRecorder(1)
	controls, err := NewCommitControl(fc, recorder)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(controls) != 1 {
		t.Fatalf("expected 1 control, got %d", len(controls))
	}
	if _, ok := controls["test-stub"]; !ok {
		t.Error("expected 'test-stub' control to be registered")
	}
}

func TestNewCommitControl_RequiredFactoryError(t *testing.T) {
	origFactories := commitControlFactories
	defer func() { commitControlFactories = origFactories }()

	commitControlFactories = []CommitControlFactory{
		{
			Name:     "required-fail",
			Required: true,
			New: func(c client.Client, r record.EventRecorder) (CommitControl, error) {
				return nil, fmt.Errorf("init failed")
			},
		},
	}

	fc := fake.NewClientBuilder().Build()
	recorder := record.NewFakeRecorder(1)
	_, err := NewCommitControl(fc, recorder)
	if err == nil {
		t.Fatal("expected error when required factory fails")
	}
}

func TestNewCommitControl_OptionalFactoryError(t *testing.T) {
	origFactories := commitControlFactories
	defer func() { commitControlFactories = origFactories }()

	commitControlFactories = []CommitControlFactory{
		{
			Name:     "optional-fail",
			Required: false,
			New: func(c client.Client, r record.EventRecorder) (CommitControl, error) {
				return nil, fmt.Errorf("init failed")
			},
		},
		{
			Name:     "good-control",
			Required: true,
			New: func(c client.Client, r record.EventRecorder) (CommitControl, error) {
				return &stubControl{}, nil
			},
		},
	}

	fc := fake.NewClientBuilder().Build()
	recorder := record.NewFakeRecorder(1)
	controls, err := NewCommitControl(fc, recorder)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(controls) != 1 {
		t.Fatalf("expected 1 control (optional skipped), got %d", len(controls))
	}
	if _, ok := controls["good-control"]; !ok {
		t.Error("expected 'good-control' to be registered")
	}
	if _, ok := controls["optional-fail"]; ok {
		t.Error("expected 'optional-fail' to be skipped")
	}
}
