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

package templateindex

import (
	"context"
	"errors"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func TestIndexExtractors(t *testing.T) {
	owner := metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: "python-agent", UID: "claim-uid", Controller: ptr.To(true)}
	version := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "a7f2k9m4p1q8r3s6t0v5", OwnerReferences: []metav1.OwnerReference{owner}}}
	foreign := version.DeepCopy()
	foreign.OwnerReferences[0].APIVersion = "foreign.example/v1"
	pool := &api.SandboxSet{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: "version-uid", Controller: ptr.To(true)}}}, Spec: api.SandboxSetSpec{EmbeddedSandboxTemplate: api.EmbeddedSandboxTemplate{TemplateRef: &api.SandboxTemplateRef{Name: version.Name}}}}
	tests := []struct {
		name    string
		extract client.IndexerFunc
		object  client.Object
		want    []string
	}{
		{"Claim owner", ClaimOwnerUID, version, []string{"claim-uid"}},
		{"foreign Claim group", ClaimOwnerUID, foreign, nil},
		{"unlabeled external reference", TemplateRefName, pool, []string{version.Name}},
		{"wrong reference object", TemplateRefName, version, nil},
		{"empty reference", TemplateRefName, &api.SandboxSet{}, nil},
		{"no owner", ClaimOwnerUID, &api.SandboxTemplate{}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.extract(test.object); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got=%v want=%v", got, test.want)
			}
		})
	}
}

// A registration recorder is required here because the existing fake client is
// not a FieldIndexer. It records only IndexField calls; it does not simulate
// informer reads, synchronization or API-server correctness.
type registrationRecorder struct {
	calls   []string
	failAt  int
	failure error
}

func (r *registrationRecorder) IndexField(_ context.Context, obj client.Object, name string, _ client.IndexerFunc) error {
	r.calls = append(r.calls, reflect.TypeOf(obj).String()+":"+name)
	if len(r.calls) == r.failAt {
		return r.failure
	}
	return nil
}
func TestRegisterIndependentViews(t *testing.T) {
	first, second := &registrationRecorder{}, &registrationRecorder{}
	if err := Register(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := Register(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if len(first.calls) != 2 || !reflect.DeepEqual(first.calls, second.calls) {
		t.Fatalf("independent registration skipped: %v / %v", first.calls, second.calls)
	}
	failure := errors.New("registration failure")
	failed := &registrationRecorder{failAt: 2, failure: failure}
	if err := Register(context.Background(), failed); !errors.Is(err, failure) || len(failed.calls) != 2 {
		t.Fatalf("error=%v calls=%v", err, failed.calls)
	}
}
