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

package sandboxcr

import (
	"errors"
	"fmt"
)

type retriableError struct {
	Message string
}

func (e retriableError) Error() string {
	return e.Message
}

func (e retriableError) Is(target error) bool {
	as := retriableError{}
	if !errors.As(target, &as) {
		return false
	}
	return as.Message == e.Message
}

func NoAvailableError(template, reason string) error {
	return retriableError{Message: fmt.Sprintf("no available sandboxes for template %s (%s)", template, reason)}
}
