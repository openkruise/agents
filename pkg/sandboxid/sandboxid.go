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

package sandboxid

import (
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sony/sonyflake/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	// LabelKey is the reserved Sandbox label containing an authoritative ID.
	LabelKey = agentsv1alpha1.LabelSandboxID
	// LegacySeparator separates namespace and name in a legacy Sandbox ID.
	LegacySeparator = "--"
	// ShortIDLength is the fixed length of an encoded 63-bit Sonyflake ID in an
	// eight-byte big-endian buffer as unpadded Base32. Length policy on top of it
	// is owned by callers.
	ShortIDLength = 13
	workerIDBits  = 18
	// WorkerIDLimit is the size of the worker-ID domain accepted by the
	// Sonyflake generator.
	WorkerIDLimit = 1 << workerIDBits
	sequenceBits  = 4
)

var shortEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

var sonyflakeEpoch = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

var validPrefixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// NewGenerator creates a generator with a 41-bit millisecond timestamp, a
// 18-bit worker ID, and a 4-bit sequence number.
func NewGenerator(workerID uint32) (func() (string, error), error) {
	flake, err := sonyflake.New(sonyflake.Settings{
		BitsSequence:  sequenceBits,
		BitsMachineID: workerIDBits,
		TimeUnit:      time.Millisecond,
		StartTime:     sonyflakeEpoch,
		MachineID: func() (int, error) {
			return int(workerID), nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create sandbox ID generator for worker %d: %w", workerID, err)
	}
	return func() (string, error) {
		// known-limit: Sonyflake holds its generator mutex while sleeping after
		// sequence exhaustion. The wait cannot observe request cancellation and can
		// last until CLOCK_REALTIME catches up after a backward step. Deployments must
		// prevent backward wall-clock steps while this process runs; replace NextID
		// with a context-aware scheduler if that invariant changes.
		id, err := flake.NextID()
		if err != nil {
			return "", fmt.Errorf("generate sandbox ID: %w", err)
		}
		if id <= 0 {
			return "", fmt.Errorf("generate sandbox ID: non-positive value %d", id)
		}
		return encodeShortID(id), nil
	}, nil
}

func encodeShortID(id int64) string {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], uint64(id))
	return strings.ToLower(shortEncoding.EncodeToString(raw[:]))
}

// Resolve returns the authoritative label value or the legacy ID when no value is set.
func Resolve(sandbox metav1.Object) string {
	if sandboxID := sandbox.GetLabels()[LabelKey]; sandboxID != "" {
		return sandboxID
	}
	return Legacy(sandbox.GetNamespace(), sandbox.GetName())
}

// Legacy returns the legacy namespace-and-name Sandbox ID.
func Legacy(namespace, name string) string {
	return namespace + LegacySeparator + name
}

// ValidatePrefix checks that prefix uses only [a-z0-9-], starts with [a-z0-9]
// when non-empty, and never contains the legacy separator so a prefixed short
// ID cannot collide with the legacy ID space. Callers own broader prefix and
// ID correctness policy.
func ValidatePrefix(prefix string) error {
	if strings.Contains(prefix, LegacySeparator) {
		return fmt.Errorf(
			"short sandbox ID prefix %q is invalid: it must not contain the legacy ID separator %q",
			prefix,
			LegacySeparator,
		)
	}
	// The rejected value and complete accepted pattern provide sufficient startup
	// diagnosis; the flag help documents the same rule in plain language.
	if prefix != "" && !validPrefixPattern.MatchString(prefix) {
		return fmt.Errorf(
			"short sandbox ID prefix %q is invalid: it must match %q",
			prefix,
			validPrefixPattern,
		)
	}
	return nil
}
