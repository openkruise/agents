/*
Copyright 2025.

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

package core

import "time"

const (
	// MaxClaimBatchSize is the maximum number of sandboxes to claim in a single reconcile cycle.
	// This prevents overwhelming the API server with too many concurrent updates
	MaxClaimBatchSize = 20

	// DefaultReplicasCount is the default number of sandboxes to claim if not specified in spec.
	DefaultReplicasCount = 1

	// ClaimRetryInterval is the interval between claim retries during the Claiming phase.
	// This balances responsiveness with API server load.
	ClaimRetryInterval = 2 * time.Second
)

const (
	// CommonControlName identifies the common control implementation
	CommonControlName = "common"
)
