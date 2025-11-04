package sandbox_controller

/*
import (
	"context"
	"testing"

	agentsv1alpha1 "gitlab.alibaba-inc.com/serverlessinfra/agents/api/v1alpha1"
	"gitlab.alibaba-inc.com/serverlessinfra/agents/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var (
	scheme *runtime.Scheme

	boxDemo = &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-box-1",
			Namespace:  "default",
			Finalizers: []string{SandboxFinalizer},
			Annotations: map[string]string{
				SandboxAnnotationEnableVKDeleteInstance: "true",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "mirrors-ssl.aliyuncs.com/centos:centos7",
						},
					},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxTerminating,
			Conditions: []metav1.Condition{
				{
					Type:               string(agentsv1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.Now(),
				},
			},
			PodInfo: agentsv1alpha1.PodInfo{
				AcsId:    "acs-2ze00987m29zidm3kiwy",
				NodeName: "virtual-kubelet-cn-beijing-d",
				PodIP:    "172.17.0.61",
			},
		},
	}
)

func TestHandleSandboxDeletion(t *testing.T) {
	box := boxDemo.DeepCopy()
	nt := metav1.Now()
	box.DeletionTimestamp = &nt
	client := fake.NewSimpleClientset(box)
	_, _ = client.ApiV1alpha1().Sandboxes(box.Namespace).Create(context.TODO(), box, v1.CreateOptions{})
	err := HandleSandboxDeletion(context.TODO(), client, box, deleteInstance)
	if err != nil {
		t.Fatalf("HandleSandboxDeletion failed: %s", err.Error())
	}
	obj, err := client.ApiV1alpha1().Sandboxes(box.Namespace).Get(context.TODO(), box.Name, v1.GetOptions{})
	if err != nil {
		t.Fatalf("Get Sandboxe failed: %s", err.Error())
	}
	if len(obj.Finalizers) > 0 {
		t.Fatalf("HandleSandboxDeletion Failed")
	}
}

func deleteInstance(ctx context.Context, acsId string) error {
	return nil
}
*/
