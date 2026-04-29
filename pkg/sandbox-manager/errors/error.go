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

package errors

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorNotFound   = ErrorCode("NotFound")
	ErrorNotAllowed = ErrorCode("NotAllowed")
	ErrorInternal   = ErrorCode("Internal")
	ErrorConflict   = ErrorCode("Conflict")
	ErrorUnknown    = ErrorCode("Unknown")
	ErrorBadRequest = ErrorCode("BadRequest")
)

type Error struct {
	Code    ErrorCode
	Message string
}

func (t *Error) Error() string {
	return fmt.Sprintf("%s: %s", t.Code, t.Message)
}

func NewError(code ErrorCode, message string) *Error {
	return &Error{
		Code:    code,
		Message: message,
	}
}

func GetErrCode(err error) ErrorCode {
	var innerErr = &Error{}
	ok := errors.As(err, &innerErr)
	if !ok {
		return ErrorUnknown
	}
	return innerErr.Code
}
