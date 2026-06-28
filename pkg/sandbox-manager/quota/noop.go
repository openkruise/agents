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

package quota

import "context"

type NoopBackend struct{}

func (NoopBackend) Acquire(context.Context, AcquireParams) error { return nil }

func (NoopBackend) Release(context.Context, string, string) error { return nil }

func (NoopBackend) ListEntries(context.Context, string) (map[string]Entry, error) {
	return map[string]Entry{}, nil
}

func (NoopBackend) Cleanup(context.Context, string) error { return nil }
