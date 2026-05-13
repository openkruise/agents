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
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/golang/protobuf/proto"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func TruncateConditionMessage(msg string) string {
	if len(msg) <= MaxConditionMessageLen {
		return msg
	}
	return msg[:MaxConditionMessageLen] + "..."
}

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

func GetControllerKey(obj client.Object) string {
	return types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}.String()
}

func GetSandboxControllerUsername() string {
	if ns := os.Getenv("SANDBOX_CONTROLLER_USERNAME"); len(ns) > 0 {
		return ns
	}
	return "system:serviceaccount:sandbox-system:sandbox-controller-manager"
}

func GetFirstNonLoopbackIP() string {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addresses {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

func IsLoopbackIP(ip string) bool {
	ipNet := net.ParseIP(ip)
	if ipNet == nil {
		return false
	}
	return ipNet.IsLoopback()
}

func GetFromInformerOrApiServer[T client.Object](ctx context.Context, target T, key client.ObjectKey,
	client client.Client, apiReader client.Reader) error {
	err := client.Get(ctx, key, target)
	if errors.IsNotFound(err) {
		klog.FromContext(ctx).V(consts.DebugLogLevel).
			Info("failed to get object from informer, try to fetch from api server", "key", key)
		err = apiReader.Get(ctx, key, target)
	}
	return err
}

func GetTemplateFromSandbox(sbx metav1.Object) string {
	tmpl := sbx.GetLabels()[agentsv1alpha1.LabelSandboxTemplate]
	if tmpl == "" {
		tmpl = sbx.GetLabels()[agentsv1alpha1.LabelSandboxPool]
	}
	return tmpl
}

// GetClusterIDHash returns a 4-character hex hash of the CLUSTER_ID environment variable.
// Returns empty string if CLUSTER_ID is not set.
func GetClusterIDHash() string {
	clusterID := os.Getenv("CLUSTER_ID")
	if clusterID == "" {
		return ""
	}
	hash := fmt.Sprintf("%x", md5.Sum([]byte(clusterID)))
	return hash[:4]
}

// GenerateSandboxName generates a K8s generateName prefix for sandbox objects.
// When SandboxMultiClusterNaming feature gate is enabled and CLUSTER_ID env is set,
// the cluster ID hash is embedded in the prefix to prevent cross-cluster naming conflicts.
// The returned prefix always ends with "-" and is truncated to 58 characters max.
func GenerateSandboxName(baseName string) string {
	generateName := fmt.Sprintf("%s-", baseName)
	if utilfeature.DefaultFeatureGate.Enabled(features.SandboxMultiClusterNaming) {
		if clusterHash := GetClusterIDHash(); clusterHash != "" {
			generateName = fmt.Sprintf("%s-%s-", baseName, clusterHash)
		}
	}
	// K8s generateName prefix must not exceed 58 characters
	if len(generateName) > 58 {
		generateName = generateName[:58]
	}
	return generateName
}
