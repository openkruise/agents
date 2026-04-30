/*
Copyright 2025 The Kruise Authors.

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

package fieldindex

import (
	"context"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	IndexNameForOwnerRefUID = "ownerRefUID"
	// IndexNameForSandboxSetTemplateRef indexes SandboxSets by the name of the
	// SandboxTemplate they reference via spec.templateRef.
	IndexNameForSandboxSetTemplateRef = "sandboxSetTemplateRef"
)

var (
	registerOnce sync.Once
)

var OwnerIndexFunc = func(obj client.Object) []string {
	var owners []string
	if controller := metav1.GetControllerOfNoCopy(obj); controller != nil {
		owners = append(owners, string(controller.UID))
	}
	return owners
}

var SandboxSetTemplateRefIndexFunc = func(obj client.Object) []string {
	sbs, ok := obj.(*agentsv1alpha1.SandboxSet)
	if !ok || sbs.Spec.TemplateRef == nil || sbs.Spec.TemplateRef.Name == "" {
		return nil
	}
	return []string{sbs.Spec.TemplateRef.Name}
}

func RegisterFieldIndexes(c cache.Cache) error {
	var err error
	registerOnce.Do(func() {
		if err = c.IndexField(context.TODO(), &agentsv1alpha1.Sandbox{}, IndexNameForOwnerRefUID, OwnerIndexFunc); err != nil {
			return
		}
		if err = c.IndexField(context.TODO(), &agentsv1alpha1.SandboxSet{}, IndexNameForSandboxSetTemplateRef, SandboxSetTemplateRefIndexFunc); err != nil {
			return
		}
	})
	return err
}
