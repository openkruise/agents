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

package cache

import "errors"

var (
	ErrSandboxNotFound = errors.New("sandbox not found in cache")
	// ErrCheckpointNotFound reports that no Checkpoint is indexed for the ID.
	// Cache read failures stay distinct so callers can avoid treating an
	// infrastructure error as absence.
	ErrCheckpointNotFound = errors.New("checkpoint not found in cache")
	// ErrSandboxIDAmbiguous reports more than one claimed Sandbox indexed under
	// one resolved ID, which only happens outside the supported metadata
	// contract (for example duplicated reserved labels written out of band).
	ErrSandboxIDAmbiguous = errors.New("multiple claimed sandboxes match sandbox ID")
)
