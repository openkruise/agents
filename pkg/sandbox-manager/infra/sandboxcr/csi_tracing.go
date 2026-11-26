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

package sandboxcr

import (
	"context"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/lifecycle/claim"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

// traceCSIMounts forwards to the neutral engine's tracing wrapper so the clone
// path reports the same span and attributes as the claim path.
func traceCSIMounts(ctx context.Context, sbx *v1alpha1.Sandbox, opts config.CSIMountOptions, rtOpts ...runtime.Option) (time.Duration, error) {
	return claim.TraceCSIMounts(ctx, sbx, opts, rtOpts...)
}
