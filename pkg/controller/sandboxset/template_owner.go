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

package sandboxset

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templateindex"
)

// resolveTemplateOwner loads only the declared controller from this reconciler's
// informer-backed client. It never switches to APIReader on a cache miss.
func (r *Reconciler) resolveTemplateOwner(ctx context.Context, sbs *api.SandboxSet, reference *api.SandboxTemplate) (*api.SandboxTemplate, error) {
	owner := templateindex.VersionOwnerReference(sbs)
	if owner == nil {
		return nil, nil
	}
	if reference != nil && reference.Name == owner.Name {
		return reference, nil
	}
	template := &api.SandboxTemplate{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: sbs.Namespace, Name: owner.Name}, template); err != nil {
		return nil, fmt.Errorf("cannot resolve template controller: %w", err)
	}
	return template, nil
}
