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

package keys

import (
	"context"

	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
)

type quotaSubjectLister struct {
	storage KeyStorage
}

func NewQuotaSubjectLister(storage KeyStorage) quotaspec.SubjectLister {
	return quotaSubjectLister{storage: storage}
}

func (l quotaSubjectLister) ListLimited(ctx context.Context) ([]quotaspec.Subject, error) {
	keys, err := l.storage.ListLimited(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]quotaspec.Subject, 0, len(keys))
	for _, key := range keys {
		if key == nil || key.QuotaSpec == nil || !key.QuotaSpec.IsLimited() {
			continue
		}
		out = append(out, quotaspec.Subject{User: key.ID.String(), Quota: key.QuotaSpec.DeepCopy()})
	}
	return out, nil
}

func (l quotaSubjectLister) Load(ctx context.Context, user string) (quotaspec.Subject, bool) {
	key, ok := l.storage.LoadByID(ctx, user)
	if !ok || key == nil || key.QuotaSpec == nil || !key.QuotaSpec.IsLimited() {
		return quotaspec.Subject{}, false
	}
	return quotaspec.Subject{User: key.ID.String(), Quota: key.QuotaSpec.DeepCopy()}, true
}
