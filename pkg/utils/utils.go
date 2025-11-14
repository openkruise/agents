package utils

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"sync"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func SetSandboxCondition(status *agentsv1alpha1.SandboxStatus, condition metav1.Condition) {
	currentCond := GetSandboxCondition(status, condition.Type)
	if currentCond != nil && currentCond.Status == condition.Status && currentCond.Reason == condition.Reason &&
		currentCond.Message == condition.Message && currentCond.LastTransitionTime == condition.LastTransitionTime {
		return
	} else if currentCond == nil {
		status.Conditions = append(status.Conditions, condition)
		return
	}
	currentCond.Status = condition.Status
	currentCond.LastTransitionTime = condition.LastTransitionTime
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
	err := c.Update(context.TODO(), fetchedObject)
	return err
}

func DumpJson(o interface{}) string {
	by, _ := json.Marshal(o)
	return string(by)
}

func InjectResumedPod(box *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[PodAnnotationAcsInstanceId] = box.Status.PodInfo.Annotations[PodAnnotationAcsInstanceId]
	pod.Annotations[PodAnnotationSourcePodUID] = box.Status.PodInfo.Annotations[PodAnnotationSourcePodUID]
	pod.Annotations[PodAnnotationSandboxPause] = True
	pod.Annotations["ProviderCreate"] = "done"
	pod.Annotations[PodAnnotationRecreating] = True
	pod.Spec.NodeName = box.Status.PodInfo.NodeName
}

var klogInitOnce sync.Once

func InitKLogOutput() {
	klogInitOnce.Do(func() {
		klog.InitFlags(nil)
		_ = flag.Set("v", fmt.Sprintf("%d", DebugLogLevel))
		flag.Parse()
	})
}
