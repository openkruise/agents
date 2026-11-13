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

package allocator

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	session "github.com/openkruise/agents/pkg/servingsession"
)

func TestAllocationRequiresFreshAdmission(t *testing.T) {
	m, c, parent := allocationFixture(t, "Strict", nil, interceptor.Funcs{}, allocationBox("first", "a"))
	admitted, err := m.store.GetServing(t.Context(), "team", "serving-id")
	if err != nil {
		t.Fatal(err)
	}
	parent.Spec.Ports = []int32{9090}
	if err := c.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	_, err = m.Allocate(t.Context(), admitted, "session")
	if !errors.Is(err, session.ErrPolicyChanged) {
		t.Fatalf("changed policy was silently adopted: %v", err)
	}
	var member agents.Sandbox
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: "first"}, &member); err != nil {
		t.Fatal(err)
	}
	if len(readAllocationBindings(t, c, "first")) != 0 {
		t.Fatal("changed policy admitted a binding")
	}
}

func TestMetadataUpdatePreservesAdmission(t *testing.T) {
	m, c, parent := allocationFixture(t, "Strict", nil, interceptor.Funcs{}, allocationBox("first", "a"))
	admitted, err := m.store.GetServing(t.Context(), "team", "serving-id")
	if err != nil {
		t.Fatal(err)
	}
	parent.Annotations = map[string]string{"example.com/status-note": "changed"}
	if err := c.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	result, err := m.Allocate(t.Context(), admitted, "session")
	if err != nil || result.Name != "first" {
		t.Fatalf("unrelated parent update invalidated admission: result=%+v err=%v", result, err)
	}
}

func TestAllocationDoesNotReturnRuntimeCredentials(t *testing.T) {
	box := allocationBox("first", "a")
	box.Annotations[agents.AnnotationRuntimeAccessToken] = "runtime-secret"
	m, _, _ := allocationFixture(t, "", nil, interceptor.Funcs{}, box)
	parent, err := m.store.GetServing(t.Context(), "team", "serving-id")
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.Allocate(t.Context(), parent, "session")
	if err != nil {
		t.Fatal(err)
	}
	if result.AccessToken != "" {
		t.Fatal("allocation result exposed runtime credential")
	}
}

func TestPolicyChangeDoesNotForgetUnknownWriteTarget(t *testing.T) {
	patches := 0
	hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		patches++
		if err := c.Patch(ctx, obj, p, opts...); err != nil {
			return err
		}
		var parent agents.SandboxServing
		if err := c.Get(ctx, client.ObjectKey{Namespace: "team", Name: "serving"}, &parent); err != nil {
			return err
		}
		parent.Spec.Ports = []int32{9090}
		if err := c.Update(ctx, &parent); err != nil {
			return err
		}
		return errors.New("binding committed but ACK lost")
	}}
	m, c, _ := allocationFixture(t, "", nil, hooks, allocationBox("first", "a"), allocationBox("second", "b"))
	parent, err := m.store.GetServing(t.Context(), "team", "serving-id")
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Allocate(t.Context(), parent, "session")
	if err == session.ErrPolicyChanged || !errors.Is(err, session.ErrServingUnavailable) {
		t.Fatalf("uncertain write allowed admission restart: %v", err)
	}
	if patches != 1 {
		t.Fatalf("uncertain target caused %d writes", patches)
	}
	if _, ok := readAllocationBindings(t, c, "first")["session"]; !ok {
		t.Fatal("committed target was lost")
	}
	if _, ok := readAllocationBindings(t, c, "second")["session"]; ok {
		t.Fatal("admission retry bound a second target")
	}
}
