package errors

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorNotFound   = ErrorCode("NotFound")
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
