package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

var TestServerPort = 9999
var Namespace = "test"
var InitKey = "admin-987654321"

func CreateSandboxWithStatus(t *testing.T, client versioned.Interface, sbx *agentsv1alpha1.Sandbox) {
	ctx := context.Background()
	_, err := client.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(ctx, sbx, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(ctx, sbx, metav1.UpdateOptions{})
	assert.NoError(t, err)
}

func CreatePodWithStatus(t *testing.T, client kubernetes.Interface, pod *corev1.Pod) {
	ctx := context.Background()
	_, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = client.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	assert.NoError(t, err)
}

func Setup(t *testing.T) (*Controller, *clients.ClientSet, func()) {
	utils.InitLogOutput()
	sandboxcr.SetClaimLockTimeout(100 * time.Millisecond)
	clientSet := clients.NewFakeClientSet()
	namespace := "sandbox-system"
	// mock self pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-manager",
			Namespace: namespace,
			Labels: map[string]string{
				"component": "sandbox-manager",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
			PodIP: "127.0.0.1",
		},
	}
	CreatePodWithStatus(t, clientSet.K8sClient, pod)

	// create key store
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keys.KeySecretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{},
	}
	_, err := clientSet.CoreV1().Secrets(namespace).Create(t.Context(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	controller := NewController("example.com", InitKey, namespace, models.DefaultMaxTimeout, TestServerPort, true, clientSet)
	assert.NoError(t, controller.Init(consts.InfraSandboxCR))
	_, err = controller.Run(namespace, "component=sandbox-manager")
	assert.NoError(t, err)
	return controller, clientSet, func() {
		controller.stop <- syscall.SIGTERM
	}
}

func NewRequest(t *testing.T, query map[string]string, body any, pathValues map[string]string, user *models.CreatedTeamAPIKey) *http.Request {
	var bodyBuf io.Reader
	if body != nil {
		marshal, err := json.Marshal(body)
		assert.NoError(t, err)
		bodyBuf = bytes.NewBuffer(marshal)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", TestServerPort)
	if query != nil {
		queryStr := "?"
		for k, v := range query {
			queryStr += fmt.Sprintf("%s=%s&", k, v)
		}
		queryStr = queryStr[:len(queryStr)-1]
		url += queryStr
	}
	req, err := http.NewRequest("", url, bodyBuf)
	assert.NoError(t, err)
	if pathValues != nil {
		for k, v := range pathValues {
			req.SetPathValue(k, v)
		}
	}
	return req.WithContext(context.WithValue(req.Context(), "user", user))
}

func GetSbsOwnerReference(sbs *agentsv1alpha1.SandboxSet) []metav1.OwnerReference {
	return []metav1.OwnerReference{*metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind)}
}

func CreateSandboxPool(t *testing.T, client versioned.Interface, name string, available int) func() {
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: Namespace,
			UID:       types.UID(uuid.NewString()),
		},
	}
	_, err := client.ApiV1alpha1().SandboxSets(Namespace).Create(context.Background(), sbs, metav1.CreateOptions{})
	assert.NoError(t, err)
	for i := 0; i < available; i++ {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", name, i),
				Namespace: Namespace,
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxPool: name,
				},
				OwnerReferences: GetSbsOwnerReference(sbs),
				ResourceVersion: "1",
				UID:             types.UID(uuid.NewString()),
			},
			Spec: agentsv1alpha1.SandboxSpec{
				SandboxTemplate: agentsv1alpha1.SandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "old-image",
								},
							},
						},
					},
				},
			},
			Status: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					},
				},
				PodInfo: agentsv1alpha1.PodInfo{
					PodIP: "1.2.3.4",
				},
			},
		}
		CreateSandboxWithStatus(t, client, sbx)
	}
	time.Sleep(100 * time.Millisecond)
	return func() {
		assert.NoError(t, client.ApiV1alpha1().SandboxSets(Namespace).Delete(context.Background(), name, metav1.DeleteOptions{}))
		for i := 0; i < available; i++ {
			assert.NoError(t, client.ApiV1alpha1().Sandboxes(Namespace).Delete(context.Background(), fmt.Sprintf("%s-%d", name, i), metav1.DeleteOptions{}))
		}
	}
}

// AvoidGetFromCache makes the resourceVersionExpectation unsatisfied to avoid getting sandbox from cache,
// which is useful in unit tests for zero-latency update
func AvoidGetFromCache(t *testing.T, sandboxID string, client clients.SandboxClient) {
	sbx := GetSandbox(t, sandboxID, client)
	sbx.ResourceVersion = "100"
	utils.ResourceVersionExpectationExpect(sbx)
}

func GetSandbox(t *testing.T, sandboxID string, client clients.SandboxClient) *agentsv1alpha1.Sandbox {
	split := strings.Split(sandboxID, "--")
	namespace, name := split[0], split[1]
	sbx, err := client.ApiV1alpha1().Sandboxes(namespace).Get(context.Background(), name, metav1.GetOptions{})
	assert.NoError(t, err)
	return sbx
}

type DoFunc func(t *testing.T, client clients.SandboxClient, sbx *agentsv1alpha1.Sandbox)
type WhenFunc func(sbx *agentsv1alpha1.Sandbox) bool

func Immediately(sbx *agentsv1alpha1.Sandbox) bool {
	return sbx != nil
}

func UpdateSandboxWhen(t *testing.T, client clients.SandboxClient, sandboxID string, when WhenFunc, do DoFunc) {
	var sbx *agentsv1alpha1.Sandbox
	if !assert.Eventually(t, func() bool {
		sbx = GetSandbox(t, sandboxID, client)
		return when(sbx)
	}, 5*time.Second, 10*time.Millisecond) {
		return
	}
	if sbx != nil {
		do(t, client, sbx.DeepCopy())
	}
}

func DoSetSandboxStatus(phase agentsv1alpha1.SandboxPhase, pausedStatus, readyStatus metav1.ConditionStatus) DoFunc {
	return func(t *testing.T, client clients.SandboxClient, sbx *agentsv1alpha1.Sandbox) {
		sbx.Status.Phase = phase
		sbx.Status.Conditions = []metav1.Condition{
			{
				Type:   string(agentsv1alpha1.SandboxConditionPaused),
				Status: pausedStatus,
			},
			{
				Type:   string(agentsv1alpha1.SandboxConditionReady),
				Status: readyStatus,
			},
		}
		_, err := client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(context.Background(), sbx, metav1.UpdateOptions{})
		assert.NoError(t, err)
	}
}

func AssertEndAt(t *testing.T, expect time.Time, endAt string) {
	endAtTime, err := time.Parse(time.RFC3339, endAt)
	assert.NoError(t, err)
	assert.WithinDuration(t, expect, endAtTime, 5*time.Second)
}
