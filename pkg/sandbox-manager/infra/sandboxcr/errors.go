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
