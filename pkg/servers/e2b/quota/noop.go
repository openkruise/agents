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

import (
	"context"
	"time"
)

type NoopBackend struct{}

func (NoopBackend) Acquire(context.Context, string, string, int64) error { return nil }

func (NoopBackend) Release(context.Context, string, string) error { return nil }

func (NoopBackend) ReleaseResult(context.Context, string, string) (bool, error) { return false, nil }

func (NoopBackend) AddObserved(context.Context, string, string, time.Time) error { return nil }

func (NoopBackend) List(context.Context, string) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

func (NoopBackend) DeleteSubject(context.Context, string) error { return nil }
