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

package tracing

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
)

// Pod lifecycle phase constants used by TraceOperation callers.
const (
	PhasePatch            = "Patch"
	PhaseCreatePod        = "Create-Pod"
	PhaseDeletePod        = "Delete-Pod"
	PhasePausePod         = "Pause-Pod"
	PhaseResumePod        = "Resume-Pod"
	PhaseAgentRuntimeInit = "Agent-Runtime-Init"
)

const maxErrorCodeLen = 256

// TraceLogEntry holds the data for a single trace span.
// Emitter implementations convert this to their output format.
type TraceLogEntry struct {
	TraceID      string
	SpanID       string
	Name         string
	Kind         string
	Phase        string
	Module       string
	StartTime    time.Time
	EndTime      time.Time
	Success      bool
	ErrorCode    string
	ResourceUID  string
	CreationTime time.Time
	// Extra holds emitter-specific attributes (e.g., cluster ID, instance ID).
	Extra map[string]string
}

func generateSpanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func classifyError(err error) string {
	if err == nil {
		return ""
	}
	if errors.IsNotFound(err) {
		return "NotFound"
	}
	if errors.IsConflict(err) {
		return "Conflict"
	}
	if errors.IsTimeout(err) || errors.IsServerTimeout(err) {
		return "Timeout"
	}
	if errors.IsAlreadyExists(err) {
		return "AlreadyExists"
	}
	s := err.Error()
	if len(s) > maxErrorCodeLen {
		s = s[:maxErrorCodeLen]
	}
	return s
}
