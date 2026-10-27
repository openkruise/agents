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

package sandbox_manager

import (
	"context"

	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/templatecr"
)

var _ TemplateStore = (*templatecr.Store)(nil)

// TemplateStore is the narrow persistence boundary for logical Template
// coordination records. Get is informer-backed; GetFresh is reserved for
// resolving write conflicts and confirming reads that may follow this
// process's own writes, where informer staleness cannot decide the outcome.
type TemplateStore interface {
	Get(ctx context.Context, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error)
	GetFresh(ctx context.Context, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error)
	Create(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error
	Update(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error
	Delete(ctx context.Context, namespace, name string, uid types.UID, resourceVersion string) error
}
