package errors

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestError(t *testing.T) {
	newError := NewError(ErrorInternal, "foo")
	code := GetErrCode(newError)
	assert.Equal(t, ErrorInternal, code)
	assert.Equal(t, "Internal: foo", newError.Error())
	assert.Equal(t, ErrorUnknown, GetErrCode(nil))
}
