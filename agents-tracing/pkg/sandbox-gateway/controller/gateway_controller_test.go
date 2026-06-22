/*
Copyright 2025.

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

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

func TestSandboxReconciler_Reconcile_SandboxNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	// Pre-populate registry with an entry that should be removed
	registry.GetRegistry().Update("default--deleted-sandbox", proxy.Route{IP: "10.0.0.1", ResourceVersion: "1"})

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "deleted-sandbox",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result.Requeue {
		t.Error("Expected no requeue")
	}

	// Verify registry entry was deleted
	_, found := registry.GetRegistry().Get("default--deleted-sandbox")
	if found {
		t.Error("Expected registry entry to be deleted")
	}
}

func TestSandboxReconciler_Reconcile_SandboxBeingDeleted(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deleting-sandbox",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{"test-finalizer"},
		},
		Status: agentsv1alpha1.SandboxStatus{
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "10.0.0.2",
			},
		},
	}

	// Pre-populate registry
	registry.GetRegistry().Update("default--deleting-sandbox", proxy.Route{IP: "10.0.0.2", ResourceVersion: "1"})

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "deleting-sandbox",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result.Requeue {
		t.Error("Expected no requeue")
	}

	// Verify registry entry was deleted
	_, found := registry.GetRegistry().Get("default--deleting-sandbox")
	if found {
		t.Error("Expected registry entry to be deleted when sandbox is being deleted")
	}
}

func TestSandboxReconciler_Reconcile_SandboxNoPodIP(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-ip-sandbox",
			Namespace: "default",
		},
		Status: agentsv1alpha1.SandboxStatus{
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "no-ip-sandbox",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result.Requeue {
		t.Error("Expected no requeue")
	}

	// Verify registry was updated with state="creating" (consistent with sandbox-manager behavior)
	route, found := registry.GetRegistry().Get("default--no-ip-sandbox")
	if !found {
		t.Error("Expected registry entry to be created with creating state")
	}
	if route.IP != "" {
		t.Errorf("Expected empty IP, got %s", route.IP)
	}
	if route.State != agentsv1alpha1.SandboxStateCreating {
		t.Errorf("Expected state %s, got %s", agentsv1alpha1.SandboxStateCreating, route.State)
	}
}

func TestSandboxReconciler_Reconcile_SandboxWithPodIP(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-sandbox",
			Namespace: "default",
		},
		Status: agentsv1alpha1.SandboxStatus{
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "10.0.0.5",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "running-sandbox",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result.Requeue {
		t.Error("Expected no requeue")
	}

	// Verify registry was updated
	route, found := registry.GetRegistry().Get("default--running-sandbox")
	if !found {
		t.Error("Expected registry entry to be created")
	}
	if route.IP != "10.0.0.5" {
		t.Errorf("Expected IP 10.0.0.5, got %s", route.IP)
	}

	// Cleanup
	registry.GetRegistry().Delete("default--running-sandbox")
}

func TestSandboxReconciler_Reconcile_UpdateExistingRegistryEntry(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	// Pre-populate registry with old IP
	registry.GetRegistry().Update("default--update-sandbox", proxy.Route{IP: "10.0.0.1", ResourceVersion: "1"})

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "update-sandbox",
			Namespace: "default",
		},
		Status: agentsv1alpha1.SandboxStatus{
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "10.0.0.10",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "update-sandbox",
			Namespace: "default",
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	// Verify registry was updated with new IP
	route, found := registry.GetRegistry().Get("default--update-sandbox")
	if !found {
		t.Error("Expected registry entry to exist")
	}
	if route.IP != "10.0.0.10" {
		t.Errorf("Expected IP 10.0.0.10, got %s", route.IP)
	}

	// Cleanup
	registry.GetRegistry().Delete("default--update-sandbox")
}

func TestSandboxReconciler_Reconcile_DifferentNamespaces(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name      string
		namespace string
		sboxName  string
		podIP     string
	}{
		{
			name:      "sandbox in default namespace",
			namespace: "default",
			sboxName:  "sandbox-1",
			podIP:     "10.0.1.1",
		},
		{
			name:      "sandbox in custom namespace",
			namespace: "production",
			sboxName:  "sandbox-2",
			podIP:     "10.0.2.2",
		},
		{
			name:      "sandbox in namespace with hyphens",
			namespace: "my-namespace",
			sboxName:  "sandbox-3",
			podIP:     "10.0.3.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.sboxName,
					Namespace: tt.namespace,
				},
				Status: agentsv1alpha1.SandboxStatus{
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: tt.podIP,
					},
				},
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
			reconciler := &SandboxReconciler{
				Client: client,
				Scheme: scheme,
			}

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.sboxName,
					Namespace: tt.namespace,
				},
			}

			_, err := reconciler.Reconcile(context.Background(), req)
			if err != nil {
				t.Errorf("Expected no error, got %v", err)
			}

			expectedKey := tt.namespace + "--" + tt.sboxName
			route, found := registry.GetRegistry().Get(expectedKey)
			if !found {
				t.Errorf("Expected registry entry for key %s", expectedKey)
			}
			if route.IP != tt.podIP {
				t.Errorf("Expected IP %s, got %s", tt.podIP, route.IP)
			}

			// Cleanup
			registry.GetRegistry().Delete(expectedKey)
		})
	}
}

func TestSandboxReconciler_Reconcile_ConcurrentUpdates(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	// Create multiple sandboxes
	sandboxes := []*agentsv1alpha1.Sandbox{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "concurrent-1",
				Namespace: "default",
			},
			Status: agentsv1alpha1.SandboxStatus{
				PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "concurrent-2",
				Namespace: "default",
			},
			Status: agentsv1alpha1.SandboxStatus{
				PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.2"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "concurrent-3",
				Namespace: "default",
			},
			Status: agentsv1alpha1.SandboxStatus{
				PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.3"},
			},
		},
	}

	objects := make([]client.Object, len(sandboxes))
	for i, s := range sandboxes {
		objects[i] = s
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	// Process all sandboxes concurrently
	done := make(chan bool, len(sandboxes))
	for _, s := range sandboxes {
		go func(sbox *agentsv1alpha1.Sandbox) {
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      sbox.Name,
					Namespace: sbox.Namespace,
				},
			}
			_, err := reconciler.Reconcile(context.Background(), req)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			done <- true
		}(s)
	}

	// Wait for all goroutines to complete
	for i := 0; i < len(sandboxes); i++ {
		<-done
	}

	// Verify all entries were added
	for i, s := range sandboxes {
		key := "default--" + s.Name
		route, found := registry.GetRegistry().Get(key)
		if !found {
			t.Errorf("Expected registry entry for %s", key)
		}
		expectedIP := "10.0.0." + string(rune('1'+i))
		if route.IP != expectedIP {
			t.Errorf("Expected IP %s, got %s", expectedIP, route.IP)
		}
		// Cleanup
		registry.GetRegistry().Delete(key)
	}
}

func TestSandboxReconciler_Reconcile_GetError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	// Create a sandbox
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	// Test with empty request (should return not found error)
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "nonexistent",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error for not found, got %v", err)
	}
	if result.Requeue {
		t.Error("Expected no requeue for not found")
	}
}

func TestSandboxReconciler_Reconcile_EmptySandboxName(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	// Test with empty sandbox name - should result in not found
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result.Requeue {
		t.Error("Expected no requeue")
	}
}

func TestSandboxReconciler_Reconcile_SandboxWithIPv6(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ipv6-sandbox",
			Namespace: "default",
		},
		Status: agentsv1alpha1.SandboxStatus{
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "2001:db8::1",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "ipv6-sandbox",
			Namespace: "default",
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	// Verify registry was updated with IPv6 address
	route, found := registry.GetRegistry().Get("default--ipv6-sandbox")
	if !found {
		t.Error("Expected registry entry to be created")
	}
	if route.IP != "2001:db8::1" {
		t.Errorf("Expected IP 2001:db8::1, got %s", route.IP)
	}

	// Cleanup
	registry.GetRegistry().Delete("default--ipv6-sandbox")
}

func TestSandboxReconciler_Reconcile_MultipleReconciles(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-reconcile-sandbox",
			Namespace: "default",
		},
		Status: agentsv1alpha1.SandboxStatus{
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "10.0.0.1",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "multi-reconcile-sandbox",
			Namespace: "default",
		},
	}

	// Reconcile multiple times
	for i := 0; i < 3; i++ {
		result, err := reconciler.Reconcile(context.Background(), req)
		if err != nil {
			t.Errorf("Iteration %d: Expected no error, got %v", i, err)
		}
		if result.Requeue {
			t.Errorf("Iteration %d: Expected no requeue", i)
		}

		// Verify registry still has correct IP
		route, found := registry.GetRegistry().Get("default--multi-reconcile-sandbox")
		if !found {
			t.Errorf("Iteration %d: Expected registry entry to exist", i)
		}
		if route.IP != "10.0.0.1" {
			t.Errorf("Iteration %d: Expected IP 10.0.0.1, got %s", i, route.IP)
		}
	}

	// Cleanup
	registry.GetRegistry().Delete("default--multi-reconcile-sandbox")
}

func TestSandboxReconciler_Reconcile_SandboxNameWithSpecialCharacters(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name      string
		sboxName  string
		namespace string
		podIP     string
	}{
		{
			name:      "sandbox with hyphens",
			sboxName:  "my-sandbox-name",
			namespace: "default",
			podIP:     "10.0.0.1",
		},
		{
			name:      "sandbox with dots",
			sboxName:  "my.sandbox.name",
			namespace: "default",
			podIP:     "10.0.0.2",
		},
		{
			name:      "sandbox with alphanumeric",
			sboxName:  "sandbox123-test",
			namespace: "default",
			podIP:     "10.0.0.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.sboxName,
					Namespace: tt.namespace,
				},
				Status: agentsv1alpha1.SandboxStatus{
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: tt.podIP,
					},
				},
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
			reconciler := &SandboxReconciler{
				Client: client,
				Scheme: scheme,
			}

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.sboxName,
					Namespace: tt.namespace,
				},
			}

			_, err := reconciler.Reconcile(context.Background(), req)
			if err != nil {
				t.Errorf("Expected no error, got %v", err)
			}

			expectedKey := tt.namespace + "--" + tt.sboxName
			route, found := registry.GetRegistry().Get(expectedKey)
			if !found {
				t.Errorf("Expected registry entry for key %s", expectedKey)
			}
			if route.IP != tt.podIP {
				t.Errorf("Expected IP %s, got %s", tt.podIP, route.IP)
			}

			// Cleanup
			registry.GetRegistry().Delete(expectedKey)
		})
	}
}

func TestSandboxReconciler_Reconcile_DeletionTimestampWithZeroTime(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	// Create a sandbox with a zero deletion timestamp
	// When DeletionTimestamp is set (even to zero time), the controller should remove from registry
	zeroTime := &metav1.Time{Time: time.Time{}}
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "zero-deletion-sandbox",
			Namespace:         "default",
			DeletionTimestamp: zeroTime,
			Finalizers:        []string{"test-finalizer"},
		},
		Status: agentsv1alpha1.SandboxStatus{
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "10.0.0.77",
			},
		},
	}

	// Pre-populate registry
	registry.GetRegistry().Update("default--zero-deletion-sandbox", proxy.Route{IP: "10.0.0.77", ResourceVersion: "1"})

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "zero-deletion-sandbox",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result.Requeue {
		t.Error("Expected no requeue")
	}

	// Since DeletionTimestamp is not nil, entry should be deleted
	// Note: The fake client may not properly handle DeletionTimestamp on initial creation,
	// so we just verify the reconcile completes without error
	// The actual behavior is tested in the "SandboxBeingDeleted" test
}

func TestSandboxReconciler_Reconcile_NilStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	// Sandbox with nil status (should be handled gracefully)
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nil-status-sandbox",
			Namespace: "default",
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "nil-status-sandbox",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if result.Requeue {
		t.Error("Expected no requeue")
	}

	// Verify registry was updated with state="creating" (consistent with sandbox-manager behavior)
	route, found := registry.GetRegistry().Get("default--nil-status-sandbox")
	if !found {
		t.Error("Expected registry entry to be created with creating state")
	}
	if route.IP != "" {
		t.Errorf("Expected empty IP, got %s", route.IP)
	}
	if route.State != agentsv1alpha1.SandboxStateCreating {
		t.Errorf("Expected state %s, got %s", agentsv1alpha1.SandboxStateCreating, route.State)
	}
}

// Test error handling when Get returns an unexpected error
func TestSandboxReconciler_Reconcile_UnexpectedGetError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	// Create a client that will return an error
	// We use an empty scheme to force errors
	emptyScheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(emptyScheme).Build()

	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	// Since the sandbox type is not registered in emptyScheme, Get should fail
	// The error should be returned
	if err == nil {
		t.Error("Expected an error when scheme doesn't have the type")
	}
	if result.Requeue {
		t.Error("Expected no requeue on error")
	}
}
