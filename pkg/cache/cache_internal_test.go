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

package cache

import (
	"errors"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// probeMgr is a tiny ctrl.Manager that records and reports back the
// AddHealthzCheck / AddReadyzCheck calls made by registerProbeChecks. Other
// methods inherit from the embedded nil interface and will panic if called —
// the helper under test only touches the two we override.
type probeMgr struct {
	ctrl.Manager // intentionally nil; do not call other methods
	healthzAdded int
	readyzAdded  int
	healthzErr   error
	readyzErr    error
}

func (p *probeMgr) AddHealthzCheck(_ string, _ healthz.Checker) error {
	p.healthzAdded++
	return p.healthzErr
}

func (p *probeMgr) AddReadyzCheck(_ string, _ healthz.Checker) error {
	p.readyzAdded++
	return p.readyzErr
}

func TestRegisterProbeChecks(t *testing.T) {
	t.Run("no-op when addr is empty", func(t *testing.T) {
		mgr := &probeMgr{}
		if err := registerProbeChecks(mgr, ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mgr.healthzAdded != 0 || mgr.readyzAdded != 0 {
			t.Errorf("expected no checks when addr is empty, got h=%d r=%d",
				mgr.healthzAdded, mgr.readyzAdded)
		}
	})

	t.Run("registers both checks when addr is set", func(t *testing.T) {
		mgr := &probeMgr{}
		if err := registerProbeChecks(mgr, ":8081"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mgr.healthzAdded != 1 {
			t.Errorf("AddHealthzCheck calls = %d, want 1", mgr.healthzAdded)
		}
		if mgr.readyzAdded != 1 {
			t.Errorf("AddReadyzCheck calls = %d, want 1", mgr.readyzAdded)
		}
	})

	t.Run("propagates AddHealthzCheck error", func(t *testing.T) {
		want := errors.New("boom-healthz")
		mgr := &probeMgr{healthzErr: want}
		err := registerProbeChecks(mgr, ":8081")
		if err == nil || !errors.Is(err, want) {
			t.Errorf("expected wrapped error from AddHealthzCheck, got %v", err)
		}
		if mgr.readyzAdded != 0 {
			t.Error("AddReadyzCheck should not be called when AddHealthzCheck fails")
		}
	})

	t.Run("propagates AddReadyzCheck error", func(t *testing.T) {
		want := errors.New("boom-readyz")
		mgr := &probeMgr{readyzErr: want}
		err := registerProbeChecks(mgr, ":8081")
		if err == nil || !errors.Is(err, want) {
			t.Errorf("expected wrapped error from AddReadyzCheck, got %v", err)
		}
		if mgr.healthzAdded != 1 {
			t.Errorf("AddHealthzCheck should still have been called once, got %d", mgr.healthzAdded)
		}
	})
}
