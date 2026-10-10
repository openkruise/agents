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

package expectations

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

func TestScale(t *testing.T) {
	e := NewScaleExpectations()
	controllerKey01 := "default/cs01"
	controllerKey02 := "default/cs02"
	pod01 := "pod01"
	pod02 := "pod02"

	e.ExpectScale(controllerKey01, Create, pod01)
	e.ExpectScale(controllerKey01, Create, pod02)
	e.ExpectScale(controllerKey01, Delete, pod01)
	if ok, _, _ := e.SatisfiedExpectations(controllerKey01); ok {
		t.Fatalf("expected not satisfied")
	}

	e.ObserveScale(controllerKey01, Create, pod02)
	e.ObserveScale(controllerKey01, Create, pod01)
	if ok, _, _ := e.SatisfiedExpectations(controllerKey01); ok {
		t.Fatalf("expected not satisfied")
	}

	e.ObserveScale(controllerKey02, Delete, pod01)
	if ok, _, _ := e.SatisfiedExpectations(controllerKey01); ok {
		t.Fatalf("expected not satisfied")
	}

	e.ObserveScale(controllerKey01, Delete, pod01)
	if ok, _, _ := e.SatisfiedExpectations(controllerKey01); !ok {
		t.Fatalf("expected satisfied")
	}
}

func TestScaleGetExpectations(t *testing.T) {
	e := NewScaleExpectations()
	key := "default/cs01"

	if got := e.GetExpectations(key); got != nil {
		t.Fatalf("expected nil for an unknown controller key, got %v", got)
	}

	e.ExpectScale(key, Create, "pod01")
	e.ExpectScale(key, Create, "pod02")
	e.ExpectScale(key, Delete, "pod03")

	got := e.GetExpectations(key)
	if got[Create].Len() != 2 || !got[Create].Has("pod01") || !got[Create].Has("pod02") {
		t.Fatalf("create expectations wrong: %v", got[Create].List())
	}
	if got[Delete].Len() != 1 || !got[Delete].Has("pod03") {
		t.Fatalf("delete expectations wrong: %v", got[Delete].List())
	}

	// The returned sets are copies. A caller that mutates them must not be able to
	// satisfy an expectation it never observed.
	got[Create].Delete("pod01")
	got[Create].Insert("pod99")
	got[Delete].Delete("pod03")

	again := e.GetExpectations(key)
	if again[Create].Len() != 2 || !again[Create].Has("pod01") || again[Create].Has("pod99") {
		t.Fatalf("mutating the returned set changed the stored create set: %v", again[Create].List())
	}
	if again[Delete].Len() != 1 || !again[Delete].Has("pod03") {
		t.Fatalf("mutating the returned set changed the stored delete set: %v", again[Delete].List())
	}
	if ok, _, _ := e.SatisfiedExpectations(key); ok {
		t.Fatalf("expected still unsatisfied after mutating a returned copy")
	}
}

func TestScaleDeleteExpectations(t *testing.T) {
	e := NewScaleExpectations()
	key := "default/cs01"

	// Deleting an unknown key is a no-op rather than a panic.
	e.DeleteExpectations(key)

	e.ExpectScale(key, Create, "pod01")
	e.ExpectScale(key, Delete, "pod02")
	if ok, _, _ := e.SatisfiedExpectations(key); ok {
		t.Fatalf("expected not satisfied before delete")
	}

	e.DeleteExpectations(key)

	if got := e.GetExpectations(key); got != nil {
		t.Fatalf("expected nil expectations after delete, got %v", got)
	}
	ok, dur, dirty := e.SatisfiedExpectations(key)
	if !ok || dur != 0 || dirty != nil {
		t.Fatalf("expected satisfied after delete, got ok=%v dur=%v dirty=%v", ok, dur, dirty)
	}
}

func TestScaleObserveUnrecordedAction(t *testing.T) {
	e := NewScaleExpectations()
	key := "default/cs01"

	// Observing a key that was never expected is a no-op.
	e.ObserveScale(key, Create, "pod01")
	if got := e.GetExpectations(key); got != nil {
		t.Fatalf("observing an unknown key created state: %v", got)
	}

	e.ExpectScale(key, Create, "pod01")

	// Observing an action with no recorded set must not satisfy the other action.
	e.ObserveScale(key, Delete, "pod01")
	if ok, _, _ := e.SatisfiedExpectations(key); ok {
		t.Fatalf("observing an unrecorded action satisfied the create expectation")
	}

	e.ObserveScale(key, Create, "pod01")
	if ok, _, _ := e.SatisfiedExpectations(key); !ok {
		t.Fatalf("expected satisfied after observing the create")
	}
}

// TestScaleSatisfiedClearsEmptyEntry covers the cleanup branch at the end of
// SatisfiedExpectations. The exported API cannot reach that state today: ExpectScale
// always inserts a name, and ObserveScale deletes the controller key itself once every
// action set is empty. The branch is the backstop for that invariant, so the state is
// built directly here and the test fails if the backstop stops clearing.
func TestScaleSatisfiedClearsEmptyEntry(t *testing.T) {
	e := NewScaleExpectations()
	key := "default/cs01"

	real, ok := e.(*realScaleExpectations)
	if !ok {
		t.Fatalf("NewScaleExpectations no longer returns *realScaleExpectations")
	}
	real.controllerCache[key] = &realControllerScaleExpectations{
		objsCache: map[ScaleAction]sets.String{
			Create: sets.NewString(),
			Delete: sets.NewString(),
		},
	}

	satisfied, dur, dirty := e.SatisfiedExpectations(key)
	if !satisfied || dur != 0 || dirty != nil {
		t.Fatalf("expected satisfied, got ok=%v dur=%v dirty=%v", satisfied, dur, dirty)
	}
	if _, still := real.controllerCache[key]; still {
		t.Fatalf("expected the empty entry to be cleared from the cache")
	}
}
