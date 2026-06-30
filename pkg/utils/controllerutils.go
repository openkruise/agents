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
package utils

import (
	"context"
	"crypto/md5" // #nosec G501 -- non-security short hash
	"fmt"
	"os"
	"sync"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	AddFinalizerOpType    FinalizerOpType = "Add"
	RemoveFinalizerOpType FinalizerOpType = "Remove"
)

type FinalizerOpType string

// UpdateFinalizer add/remove a finalizer from a object
func UpdateFinalizer(c client.Client, object client.Object, op FinalizerOpType, finalizer string) error {
	switch op {
	case AddFinalizerOpType, RemoveFinalizerOpType:
	default:
		panic("UpdateFinalizer Func 'op' parameter must be 'Add' or 'Remove'")
	}

	key := client.ObjectKeyFromObject(object)
	fetchedObject := object.DeepCopyObject().(client.Object)
	getErr := c.Get(context.TODO(), key, fetchedObject)
	if getErr != nil {
		return getErr
	}
	finalizers := fetchedObject.GetFinalizers()
	switch op {
	case AddFinalizerOpType:
		if controllerutil.ContainsFinalizer(fetchedObject, finalizer) {
			return nil
		}
		finalizers = append(finalizers, finalizer)
	case RemoveFinalizerOpType:
		finalizerSet := sets.NewString(finalizers...)
		if !finalizerSet.Has(finalizer) {
			return nil
		}
		finalizers = finalizerSet.Delete(finalizer).List()
	}
	fetchedObject.SetFinalizers(finalizers)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return c.Update(context.TODO(), fetchedObject)
	})
}

func PatchFinalizer(ctx context.Context, c client.Client, object client.Object, op FinalizerOpType, finalizer string) (client.Object, error) {
	switch op {
	case AddFinalizerOpType, RemoveFinalizerOpType:
	default:
		panic("UpdateFinalizer Func 'op' parameter must be 'Add' or 'Remove'")
	}
	originObj := object.DeepCopyObject().(client.Object)
	patch := client.MergeFrom(object)
	switch op {
	case AddFinalizerOpType:
		if controllerutil.ContainsFinalizer(originObj, finalizer) {
			return object, nil
		}
		controllerutil.AddFinalizer(originObj, finalizer)
	case RemoveFinalizerOpType:
		if !controllerutil.ContainsFinalizer(originObj, finalizer) {
			return object, nil
		}
		controllerutil.RemoveFinalizer(originObj, finalizer)
	}
	if err := client.IgnoreNotFound(c.Patch(ctx, originObj, patch)); err != nil {
		return nil, fmt.Errorf("failed to patch finalizer: %w", err)
	}
	return originObj, nil
}

// DoItSlowly tries to call the provided function a total of 'count' times,
// starting slow to check for errors, then speeding up if calls succeed.
//
// It groups the calls into batches, starting with a group of initialBatchSize.
// Within each batch, it may call the function multiple times concurrently.
//
// If a whole batch succeeds, the next batch may get exponentially larger.
// If there are any failures in a batch, all remaining batches are skipped
// after waiting for the current batch to complete.
//
// It returns the number of successful calls to the function.
func DoItSlowly(count int, initialBatchSize int, fn func() error) (int, error) {
	remaining := count
	successes := 0
	for batchSize := min(remaining, initialBatchSize); batchSize > 0; batchSize = min(2*batchSize, remaining) {
		errCh := make(chan error, batchSize)
		var wg sync.WaitGroup
		wg.Add(batchSize)
		for i := 0; i < batchSize; i++ {
			go func() {
				defer wg.Done()
				if err := fn(); err != nil {
					errCh <- err
				}
			}()
		}
		wg.Wait()
		curSuccesses := batchSize - len(errCh)
		successes += curSuccesses
		if len(errCh) > 0 {
			return successes, <-errCh
		}
		remaining -= batchSize
	}
	return successes, nil
}

func DoItSlowlyWithInputs[T any](inputs []T, initialBatchSize int, fn func(T) error) (int, error) {
	inputCh := make(chan T, len(inputs))
	for _, input := range inputs {
		inputCh <- input
	}
	return DoItSlowly(len(inputs), initialBatchSize, func() error {
		input := <-inputCh
		return fn(input)
	})
}

// GetControllerKey returns a string key in the format "namespace/name" for a client object.
// This is commonly used as a unique identifier for controller resources.
func GetControllerKey(obj client.Object) string {
	return types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}.String()
}

// GetSandboxControllerUsername returns the username of the sandbox controller service account.
// It checks the SANDBOX_CONTROLLER_USERNAME environment variable first, and falls back to
// the default service account if not set.
func GetSandboxControllerUsername() string {
	if ns := os.Getenv("SANDBOX_CONTROLLER_USERNAME"); len(ns) > 0 {
		return ns
	}
	return "system:serviceaccount:sandbox-system:sandbox-controller-manager"
}

// GetClusterIDHash returns a 4-character hex hash of the CLUSTER_ID environment variable.
// Returns empty string if CLUSTER_ID is not set.
// This is useful for generating cluster-unique identifiers while keeping them short.
func GetClusterIDHash() string {
	clusterID := os.Getenv("CLUSTER_ID")
	if clusterID == "" {
		return ""
	}
	hash := fmt.Sprintf("%x", md5.Sum([]byte(clusterID))) // #nosec G401 -- non-security short hash
	return hash[:4]
}
func GetFromInformerOrApiServer[T client.Object](ctx context.Context, target T, key client.ObjectKey,
	client client.Client, apiReader client.Reader) error {
	err := client.Get(ctx, key, target)
	if errors.IsNotFound(err) {
		klog.FromContext(ctx).V(DebugLogLevel).
			Info("failed to get object from informer, try to fetch from api server", "key", key)
		err = apiReader.Get(ctx, key, target)
	}
	return err
}
