package logs

import (
	"strconv"
	"sync/atomic"
)

type OperationID string

const (
	OperationIDKey    OperationID = "operation_id"
)

var operationID = atomic.Int32{}

func AssignOperationID() string {
	id := operationID.Add(1)

	return strconv.Itoa(int(id))
}