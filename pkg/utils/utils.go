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
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/uuid"

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
func DumpJson(o interface{}) string {
	by, _ := json.Marshal(o)
	return string(by)
}

func HashData(by []byte) string {
	shaSum := sha256.Sum256(by)
	hexStr := fmt.Sprintf("%x", shaSum)
	if len(hexStr) > 9 {
		hexStr = hexStr[:9]
	}
	return rand.SafeEncodeString(hexStr)
}

// RandStringN returns a random string of n lower-case alphanumeric characters.
// It is intended for generating short, non-deterministic suffixes (e.g. resource
// name suffixes) where uniqueness across reconcile cycles is required and the
// caller does not need the value to be reproducible from any input.
// If n <= 0, an empty string is returned.
func RandStringN(n int) string {
	if n <= 0 {
		return ""
	}
	return rand.String(n)
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

func GetTemplateFromSandbox(sbx metav1.Object) string {
	tmpl := sbx.GetLabels()[agentsv1alpha1.LabelSandboxTemplate]
	if tmpl == "" {
		tmpl = sbx.GetLabels()[agentsv1alpha1.LabelSandboxPool]
	}
	return tmpl
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

// FindContainer returns the pointer to the first container whose name matches.
func FindContainer(name string, containers []corev1.Container) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func LockSandbox(sbx client.Object, lock string, owner string) {
	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, 2)
	}
	annotations[agentsv1alpha1.AnnotationLock] = lock
	annotations[agentsv1alpha1.AnnotationOwner] = owner
	sbx.SetAnnotations(annotations)
}

func NewLockString() string {
	return uuid.NewString()
}

// nowFunc is the package-level clock used by sandbox state evaluation. It is
// declared as a variable so that tests can substitute a fake clock via
// SetNowFuncForTesting without depending on real wall-clock time.
var nowFunc = time.Now

// SetNowFuncForTesting replaces the package-level clock used by GetSandboxState
// (and other helpers that consult nowFunc) for the duration of a test. It
// returns a restore function that callers should defer to revert the change.
//
// Intended for tests only; production code must not call this.
func SetNowFuncForTesting(fn func() time.Time) func() {
	prev := nowFunc
	nowFunc = fn
	return func() { nowFunc = prev }
}

// GetSandboxState the state of agentsv1alpha1 Sandbox.
// NOTE: the reason is unique and hard-coded, so we can easily search the conditions of some reason when debugging.
func GetSandboxState(sbx *agentsv1alpha1.Sandbox) (state string, reason string) {
	if sbx.DeletionTimestamp != nil {
		return agentsv1alpha1.SandboxStateDead, "ResourceDeleted"
	}
	if sbx.Spec.ShutdownTime != nil && nowFunc().After(sbx.Spec.ShutdownTime.Time) {
		return agentsv1alpha1.SandboxStateDead, "ShutdownTimeReached"
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxPending {
		return agentsv1alpha1.SandboxStateCreating, "ResourcePending"
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxSucceeded {
		return agentsv1alpha1.SandboxStateDead, "ResourceSucceeded"
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxFailed {
		return agentsv1alpha1.SandboxStateDead, "ResourceFailed"
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxTerminating {
		return agentsv1alpha1.SandboxStateDead, "ResourceTerminating"
	}

	sandboxReady := IsSandboxReady(sbx)
	if IsControlledBySandboxSet(sbx) {
		if sandboxReady {
			return agentsv1alpha1.SandboxStateAvailable, "ResourceControlledBySbsAndReady"
		} else {
			return agentsv1alpha1.SandboxStateCreating, "ResourceControlledBySbsButNotReady"
		}
	} else {
		if sbx.Status.Phase == agentsv1alpha1.SandboxRunning {
			if sbx.Spec.Paused {
				return agentsv1alpha1.SandboxStatePaused, "RunningResourceClaimedAndPaused"
			} else {
				if sandboxReady {
					return agentsv1alpha1.SandboxStateRunning, "RunningResourceClaimedAndReady"
				} else {
					return agentsv1alpha1.SandboxStateDead, "RunningResourceClaimedButNotReady"
				}
			}
		} else {
			// Paused and Resuming phases are both treated as paused state
			return agentsv1alpha1.SandboxStatePaused, "NotRunningResourceClaimed"
		}
	}
}
func IsControlledBySandboxSet(sbx *agentsv1alpha1.Sandbox) bool {
	controller := metav1.GetControllerOfNoCopy(sbx)
	if controller == nil {
		return false
	}
	return controller.Kind == agentsv1alpha1.SandboxSetControllerKind.Kind &&
		// ** REMEMBER TO MODIFY THIS WHEN A NEW API VERSION(LIKE v1beta1) IS ADDED **
		controller.APIVersion == agentsv1alpha1.SandboxSetControllerKind.GroupVersion().String()
}

// sandboxIDSeparator joins namespace and name in a sandbox ID. It is the single source
// of truth for the encoding used by util functions
const sandboxIDSeparator = "--"

// GetSandboxID encodes a sandbox as "<namespace>--<name>". The encoding requires that
// the namespace itself does not contain "--"; callers that accept user-supplied
// namespaces must enforce this with ValidateNamespaceForSandboxID at the boundary.
// See pkg/servers/e2b/AGENTS.md ("Namespace Naming Constraint") for the rationale.
func GetSandboxID(sbx *agentsv1alpha1.Sandbox) string {
	return sbx.Namespace + sandboxIDSeparator + sbx.Name
}

// ValidateNamespaceForSandboxID rejects namespace names that cannot be safely embedded
// in a sandbox ID.
func ValidateNamespaceForSandboxID(namespace string) error {
	if namespace == "" {
		return fmt.Errorf("namespace must not be empty")
	}
	if strings.Contains(namespace, sandboxIDSeparator) {
		return fmt.Errorf("namespace %q must not contain %q: this sequence is reserved as the sandbox ID separator", namespace, sandboxIDSeparator)
	}
	return nil
}

// GetAccessToken resolves the agent-runtime access token from object annotations, falling back
// to the legacy envd annotation key for backwards compatibility. Accepts metav1.Object so that
// it works for both Sandbox and SandboxClaim objects.
func GetAccessToken(obj metav1.Object) string {
	if obj == nil {
		return ""
	}
	annotations := obj.GetAnnotations()
	if t := annotations[agentsv1alpha1.AnnotationRuntimeAccessToken]; t != "" {
		return t
	}
	return annotations[agentsv1alpha1.AnnotationEnvdAccessToken] // legacy
}

func IsSandboxReady(sbx *agentsv1alpha1.Sandbox) bool {
	readyCond := GetSandboxCondition(&sbx.Status, string(agentsv1alpha1.SandboxConditionReady))
	return readyCond != nil && readyCond.Status == metav1.ConditionTrue
}

// IsSandboxPausable returns true when the pausing operation will not cause any conflict.
func IsSandboxPausable(sbx *agentsv1alpha1.Sandbox) (bool, string) {
	if IsControlledBySandboxSet(sbx) {
		state, _ := GetSandboxState(sbx)
		switch state {
		case agentsv1alpha1.SandboxStateAvailable, agentsv1alpha1.SandboxStateCreating:
			return false, "SandboxStateNotAllowed"
		}
	}
	switch sbx.Status.Phase {
	case agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxPaused:
		return true, "SandboxIsRunningOrPaused"
	default:
		return false, "SandboxPhaseNotAllowed"
	}
}

// IsSandboxResumable returns true when the resuming operation will not cause any conflict.
func IsSandboxResumable(sbx *agentsv1alpha1.Sandbox) (bool, string) {
	switch sbx.Status.Phase {
	case agentsv1alpha1.SandboxRunning:
		if sbx.Spec.Paused {
			return false, "SandboxIsPausing"
		}
		return true, "SandboxIsRunning"
	case agentsv1alpha1.SandboxResuming:
		return true, "SandboxIsResuming"
	default:
	}
	if sbx.Status.Phase == agentsv1alpha1.SandboxPaused {
		pauseCond := GetSandboxCondition(&sbx.Status, string(agentsv1alpha1.SandboxConditionPaused))
		paused := pauseCond != nil && pauseCond.Status == metav1.ConditionTrue
		if paused {
			return true, "SandboxIsPaused"
		}
		return false, "SandboxIsPausing"
	}
	return false, "SandboxPhaseNotAllowed"
}

// GetTemplateSpec resolves and returns the PodTemplateSpec from the EmbeddedSandboxTemplate.
// If TemplateRef is specified, it will fetch the SandboxTemplate using the client.
func GetTemplateSpec(ctx context.Context, cli client.Client, namespace string, embedded *agentsv1alpha1.EmbeddedSandboxTemplate) (*corev1.PodTemplateSpec, error) {
	if embedded == nil {
		return nil, nil
	}
	if embedded.Template != nil {
		return embedded.Template, nil
	}
	if embedded.TemplateRef != nil {
		refTemplate := &agentsv1alpha1.SandboxTemplate{}
		err := cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: embedded.TemplateRef.Name}, refTemplate)
		if err != nil {
			return nil, err
		}
		return refTemplate.Spec.Template, nil
	}
	return nil, nil
}
