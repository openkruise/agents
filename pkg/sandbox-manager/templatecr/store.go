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

// Package templatecr stores logical Template coordination records in Kubernetes.
package templatecr

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// Store uses informer-backed reads by default and reserves live reads for
// resolving write conflicts and confirming reads that may follow this
// process's own writes, where stale observations cannot decide the result.
type Store struct {
	client         client.Client
	apiReader      func() client.Reader
	creationLabels map[string]string
}

// NewStore creates a logical Template store.
func NewStore(c client.Client, apiReader func() client.Reader) *Store {
	return &Store{client: c, apiReader: apiReader}
}

// WithCreationLabels sets the labels stamped onto every created template so an
// informer watch scoped by the deployment's sandbox label selector observes
// manager-created coordination records.
func (s *Store) WithCreationLabels(creation map[string]string) *Store {
	s.creationLabels = creation
	return s
}

// CreationLabelsFromSelector extracts the label set a manager-created logical
// Template must carry for an informer scoped by the deployment's sandbox
// label selector to observe it. Requirements that a template without labels
// already satisfies pass without stamps; single-value equality requirements
// are stamped; anything else fails so the caller can disable the lifecycle
// instead of serving invisible templates.
func CreationLabelsFromSelector(selector string) (map[string]string, error) {
	if selector == "" {
		return nil, nil
	}
	parsed, err := labels.Parse(selector)
	if err != nil {
		return nil, fmt.Errorf("parse sandbox label selector %q: %w", selector, err)
	}
	requirements, _ := parsed.Requirements()
	creation := map[string]string{}
	for _, requirement := range requirements {
		if requirement.Matches(labels.Set{}) {
			continue
		}
		values := requirement.Values()
		if values.Len() != 1 || !equalityOperator(requirement.Operator()) {
			return nil, fmt.Errorf("requirement %q is not satisfiable by created templates", requirement.String())
		}
		creation[requirement.Key()] = values.UnsortedList()[0]
	}
	if !parsed.Matches(labels.Set(creation)) {
		return nil, fmt.Errorf("sandbox label selector %q is not satisfiable by created templates", selector)
	}
	return creation, nil
}

func equalityOperator(operator selection.Operator) bool {
	return operator == selection.Equals || operator == selection.DoubleEquals || operator == selection.In
}

// Get returns the informer-backed view of a logical Template coordination CR.
func (s *Store) Get(ctx context.Context, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	return get(ctx, s.client, namespace, name)
}

// GetFresh reads a logical Template directly from the API server. Callers use
// it only after a write conflict proves that the informer view may be stale.
func (s *Store) GetFresh(ctx context.Context, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	return get(ctx, s.apiReader(), namespace, name)
}

func get(ctx context.Context, reader client.Reader, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	template := &agentsv1alpha1.SandboxTemplate{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, template); err != nil {
		return nil, err
	}
	return template, nil
}

// Create persists a logical Template coordination CR, stamping the creation
// labels that keep the template inside the informer watch scope.
func (s *Store) Create(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error {
	if len(s.creationLabels) > 0 {
		if template.Labels == nil {
			template.Labels = map[string]string{}
		}
		for key, value := range s.creationLabels {
			template.Labels[key] = value
		}
	}
	return s.client.Create(ctx, template)
}

// Update atomically replaces a logical Template using its resourceVersion.
func (s *Store) Update(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error {
	return s.client.Update(ctx, template)
}

// Delete removes the bound CR only when its UID and observed resourceVersion still match.
func (s *Store) Delete(ctx context.Context, namespace, name string, uid types.UID, resourceVersion string) error {
	template := &agentsv1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	preconditions := &metav1.Preconditions{UID: &uid}
	if resourceVersion != "" {
		preconditions.ResourceVersion = &resourceVersion
	}
	return s.client.Delete(ctx, template, &client.DeleteOptions{Preconditions: preconditions})
}
