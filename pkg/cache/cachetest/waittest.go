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

package cachetest

import (
	"context"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
)

// NewAdHocTask constructs a WaitTask with an arbitrary (action, checker) pair.
// Tests that exercise the low-level waitHooks semantics (action conflict,
// timeout, cancel, double-check) use this helper to keep the production
// factories immutable.
func NewAdHocTask[T client.Object](
	ctx context.Context,
	waitHooks *sync.Map,
	action cacheutils.WaitAction,
	obj T,
	update cacheutils.UpdateFunc[T],
	check cacheutils.CheckFunc[T],
) *cacheutils.WaitTask[T] {
	return cacheutils.NewWaitTask[T](ctx, waitHooks, action, obj, update, check)
}
