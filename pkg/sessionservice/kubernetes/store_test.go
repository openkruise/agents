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

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	agents "github.com/openkruise/agents/api/v1alpha1"
	session "github.com/openkruise/agents/pkg/sessionservice"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func newSessionServiceSessions(c client.Client, healthy func() bool) *Store {
	return NewStore(c, healthy)
}

func servingSessionFixture(t *testing.T) (*agents.SessionService, *agents.Sandbox, client.WithWatch) {
	t.Helper()
	parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "serving", UID: "serving-uid"}}
	box := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "member", UID: "member-uid", Labels: map[string]string{sessionbinding.SessionServiceUIDLabel: string(parent.UID)}, Annotations: map[string]string{"example.com/keep": "yes", sessionbinding.BindingsAnnotation: `{"000000000000000000000a":{"createdAt":"2026-11-10T00:00:00Z"},"000000000000000000000b":{"createdAt":"2026-11-10T00:00:00Z"}}`}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, agents.GroupVersion.WithKind("SessionService"))}}, Status: agents.SandboxStatus{Phase: agents.SandboxFailed}}
	scheme := runtime.NewScheme()
	if err := agents.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, box).Build()
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(box), box); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		expectations.ResourceVersionExpectationDelete(box)
		// Fake clients have no informer to retire expectations for extra members.
		var members agents.SandboxList
		if err := c.List(context.Background(), &members); err != nil {
			t.Error(err)
			return
		}
		for i := range members.Items {
			expectations.ResourceVersionExpectationDelete(&members.Items[i])
		}
	})
	return parent, box, c
}

func TestTemplateLabelsDoNotRestrictMemberReads(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			parent, _, c := servingSessionFixture(t)
			if !missing {
				parent.Spec.SandboxTemplateSpec = agents.SandboxTemplateSpec{Template: &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"scope": "00000000000000000other"}}}}
			}
			if err := c.Update(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			backend := NewStore(c, func() bool { return true })
			snapshot, err := backend.ReadSessionServiceSnapshot(t.Context(), servingValue(parent))
			if err != nil || len(snapshot.Members) != 1 {
				t.Fatalf("read visible members: snapshot=%+v err=%v", snapshot, err)
			}
		})
	}
}

func TestGetSessionServiceByName(t *testing.T) {
	for _, tc := range []struct {
		name, namespace, parentName string
		want                        error
	}{
		{name: "matching namespace and name", namespace: "team", parentName: "serving"},
		{name: "different namespace", namespace: "other", parentName: "serving", want: session.ErrSessionServiceNotFound},
		{name: "different name", namespace: "team", parentName: "other", want: session.ErrSessionServiceNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, _, c := servingSessionFixture(t)
			got, err := NewStore(c, func() bool { return true }).GetSessionService(t.Context(), tc.namespace, tc.parentName)
			if !errors.Is(err, tc.want) {
				t.Fatalf("lookup error=%v want=%v", err, tc.want)
			}
			if err == nil && (got.Namespace != parent.Namespace || got.Name != parent.Name || got.UID != string(parent.UID)) {
				t.Fatalf("incorrect parent identity: %+v", got)
			}
		})
	}
}
