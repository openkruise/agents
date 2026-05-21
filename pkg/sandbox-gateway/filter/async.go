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

package filter

import (
	"context"
	"strconv"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
)

func (f *sandboxFilter) wakeContext() context.Context {
	f.contextMu.Lock()
	defer f.contextMu.Unlock()

	if f.cancel == nil {
		f.context, f.cancel = context.WithCancel(context.Background())
	}
	return f.context
}

func (f *sandboxFilter) OnDestroy(api.DestroyReason) {
	// Claim the completeOnce slot so any in-flight wake goroutine that finishes
	// after Envoy destroys the filter cannot invoke callbacks on freed memory.
	f.completeOnce.Do(func() {})
	f.cancelWakeContext()
}

func (f *sandboxFilter) cancelWakeContext() {
	f.contextMu.Lock()
	cancel := f.cancel
	f.contextMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (f *sandboxFilter) completeWithReply(status int, body string, headers map[string]string, code string) {
	if status == 0 {
		return
	}
	f.completeOnce.Do(func() {
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(status, body, toReplyHeaders(headers), -1, code)
	})
}

func (f *sandboxFilter) completeWithContinue(upstreamHost string) {
	f.completeOnce.Do(func() {
		f.callbacks.StreamInfo().DynamicMetadata().Set("envoy.lb.original_dst", "host", upstreamHost)
		f.callbacks.DecoderFilterCallbacks().Continue(api.Continue)
	})
}

func toRetryAfterHeader(retryAfter int) map[string]string {
	if retryAfter < 0 {
		return nil
	}
	return map[string]string{"Retry-After": strconv.Itoa(retryAfter)}
}

func toReplyHeaders(headers map[string]string) map[string][]string {
	if len(headers) == 0 {
		return nil
	}
	result := make(map[string][]string, len(headers))
	for k, v := range headers {
		result[k] = []string{v}
	}
	return result
}
