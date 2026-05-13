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

package utils

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// ErrWaitNotSatisfied matches wait failures where the object did not reach
	// the requested state before the wait completed.
	ErrWaitNotSatisfied = &WaitNotSatisfiedError{}
	// ErrWaitTaskConflict matches failures caused by another wait action
	// already registered for the same object.
	ErrWaitTaskConflict = &WaitTaskConflictError{}
)

// WaitNotSatisfiedError reports that a wait task finished while the object was
// still not in the requested state.
type WaitNotSatisfiedError struct {
	Object            client.ObjectKey
	Action            WaitAction
	DuringDoubleCheck bool
}

func (e *WaitNotSatisfiedError) Error() string {
	if e != nil && e.DuringDoubleCheck {
		return "object is not satisfied during double check"
	}
	return "object is not satisfied"
}

func (e *WaitNotSatisfiedError) Is(target error) bool {
	_, ok := target.(*WaitNotSatisfiedError)
	return ok
}

// WaitTaskConflictError reports that a wait task cannot be registered because
// another action is already waiting on the same object.
type WaitTaskConflictError struct {
	ExistingAction WaitAction
	NewAction      WaitAction
}

func (e *WaitTaskConflictError) Error() string {
	if e == nil {
		return "wait task conflict"
	}
	return fmt.Sprintf("another action(%s)'s wait task already exists", e.ExistingAction)
}

func (e *WaitTaskConflictError) Is(target error) bool {
	_, ok := target.(*WaitTaskConflictError)
	return ok
}
