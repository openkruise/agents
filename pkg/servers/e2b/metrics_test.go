package e2b

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSnapshotMetricsRegistered(t *testing.T) {
	assert.NotNil(t, SnapshotDuration)
	assert.NotNil(t, SnapshotTotal)
}
