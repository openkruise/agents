package configuration

import (
	"reflect"
	"testing"

	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	scheme *runtime.Scheme
)

func init() {
	scheme = runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
}

func TestGetSandboxResumeAcsPodPersistentContent(t *testing.T) {
	cases := []struct {
		name         string
		getConfigmap func() *corev1.ConfigMap
		expect       func() *SandboxResumeAcsPodPersistentContent
	}{
		{
			name: "get SandboxResumeAcsPodPersistentContent",
			getConfigmap: func() *corev1.ConfigMap {
				obj := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: utils.GetAgentSandboxNamespace(),
						Name:      AgentSandboxConfigurationName,
					},
					Data: map[string]string{
						SandboxResumeAcsPodPersistentContentKey: `{"annotationKeys":["ProviderCreate","alibabacloud.com/cpu-vendors","alibabacloud.com/instance-id","alibabacloud.com/pod-ephemeral-storage"],"labelKeys":["alibabacloud.com/acs","alibabacloud.com/compute-class","alibabacloud.com/compute-qos"],"tolerationKeys":["virtual-kubelet.io/provider","node.kubernetes.io/not-ready","node.kubernetes.io/unreachable"],"whetherNodeName":true}`,
					},
				}
				return obj
			},
			expect: func() *SandboxResumeAcsPodPersistentContent {
				obj := &SandboxResumeAcsPodPersistentContent{
					AnnotationKeys: []string{
						"ProviderCreate",
						"alibabacloud.com/cpu-vendors",
						"alibabacloud.com/instance-id",
						"alibabacloud.com/pod-ephemeral-storage",
					},
					LabelKeys: []string{
						"alibabacloud.com/acs",
						"alibabacloud.com/compute-class",
						"alibabacloud.com/compute-qos",
					},
					WhetherNodeName: true,
				}
				return obj
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(cs.getConfigmap()).
				Build()

			content, err := GetSandboxResumeAcsPodPersistentContent(fakeClient)
			if err != nil {
				t.Fatalf("GetSandboxResumeAcsPodPersistentContent failed: %s", err.Error())
			}
			if !reflect.DeepEqual(content, cs.expect()) {
				t.Fatalf("expect(%s), but get(%s)", utils.DumpJson(cs.expect()), utils.DumpJson(content))
			}
		})
	}
}
