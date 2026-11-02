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

package claim

import (
	"math"
	"time"
)

var (
	// DefaultClaimTimeout bounds a single claim attempt when the caller leaves
	// ClaimTimeout unset.
	DefaultClaimTimeout = time.Minute
	// DefaultReserveFailedSandboxFor is the backend default for how long a
	// failed sandbox is kept before deletion.
	DefaultReserveFailedSandboxFor = ReserveFailedSandboxNever
	// DefaultCleanupTimeout bounds the detached context used to clean up or
	// reserve a failed sandbox after a claim error.
	DefaultCleanupTimeout = 30 * time.Second
)

// quotaReleaseTimeout bounds the detached context used to release admission
// after a failed claim so a slow quota backend cannot block cleanup.
const quotaReleaseTimeout = SandboxAdmissionReleaseTimeout

// CSIMountCountLimit caps the number of CSI mounts allowed in a single batch.
// It defaults to math.MaxInt (no limit). Tests lower it to exercise the guard.
// It is exported so the sandbox-manager clone path shares one limit definition.
var CSIMountCountLimit = math.MaxInt
