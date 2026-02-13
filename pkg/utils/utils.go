package utils

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/golang/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func SetSandboxCondition(status *agentsv1alpha1.SandboxStatus, condition metav1.Condition) {
	currentCond := GetSandboxCondition(status, condition.Type)
	if currentCond != nil && currentCond.Status == condition.Status && currentCond.Reason == condition.Reason &&
		currentCond.Message == condition.Message {
		return
	} else if currentCond == nil {
		status.Conditions = append(status.Conditions, condition)
		return
	}
	if currentCond.Status != condition.Status {
		currentCond.LastTransitionTime = condition.LastTransitionTime
	}
	currentCond.Status = condition.Status
	currentCond.Reason = condition.Reason
	currentCond.Message = condition.Message
}

func GetSandboxCondition(status *agentsv1alpha1.SandboxStatus, condType string) *metav1.Condition {
	for i := range status.Conditions {
		c := &status.Conditions[i]
		if c.Type == condType {
			return c
		}
	}
	return nil
}
func GetPodCondition(status *corev1.PodStatus, condType corev1.PodConditionType) *corev1.PodCondition {
	for i := range status.Conditions {
		c := &status.Conditions[i]
		if c.Type == condType {
			return c
		}
	}
	return nil
}

func RemoveSandboxCondition(status *agentsv1alpha1.SandboxStatus, condType string) {
	status.Conditions = filterOutCondition(status.Conditions, condType)
}

// filterOutCondition returns a new slice of rollout conditions without conditions with the provided type.
func filterOutCondition(conditions []metav1.Condition, condType string) []metav1.Condition {
	var newConditions []metav1.Condition
	for _, c := range conditions {
		if c.Type == condType {
			continue
		}
		newConditions = append(newConditions, c)
	}
	return newConditions
}

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

func DumpJson(o interface{}) string {
	by, _ := json.Marshal(o)
	return string(by)
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

func HashData(by []byte) string {
	shaSum := sha256.Sum256(by)
	hexStr := fmt.Sprintf("%x", shaSum)
	if len(hexStr) > 9 {
		hexStr = hexStr[:9]
	}
	return rand.SafeEncodeString(hexStr)
}

func EncodeBase64Proto[T proto.Message](data T) (string, error) {
	marshal, err := proto.Marshal(data)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(marshal), nil
}

func DecodeBase64Proto[T proto.Message](raw string, into T) error {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return err
	}
	return proto.Unmarshal(decoded, into)
}

func GetStringPtr(s string) *string {
	return &s
}

func StringToSlice(s, sep string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, sep)
}
