package infra

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/diff"
	"k8s.io/utils/ptr"
)

type FakePool struct {
	template *SandboxTemplate
}

func (f *FakePool) GetTemplate() *SandboxTemplate {
	return f.template
}

func (f *FakePool) ClaimSandbox(context.Context, string, func(sbx Sandbox)) (Sandbox, error) {
	return nil, nil
}

type FakeInfra struct {
	BaseInfra
}

func (f *FakeInfra) InjectTemplateMetadata() metav1.ObjectMeta {
	return metav1.ObjectMeta{}
}

func (f *FakeInfra) Run(context.Context) error {
	return nil
}

func (f *FakeInfra) Stop() {
	return
}

func (f *FakeInfra) NewPoolFromTemplate(template *SandboxTemplate) SandboxPool {
	return &FakePool{template: template}
}

func (f *FakeInfra) LoadDebugInfo() map[string]any {
	info := make(map[string]any)
	f.Pools.Range(func(key, value any) bool {
		info[value.(*FakePool).GetTemplate().Name] = struct{}{}
		return true
	})
	return info
}

func (f *FakeInfra) SelectSandboxes(SandboxSelectorOptions) ([]Sandbox, error) {
	return nil, nil
}

func (f *FakeInfra) GetSandbox(string) (Sandbox, error) {
	return nil, nil
}

func TestSandboxTemplate_Init(t *testing.T) {
	tests := []struct {
		name              string
		minPoolSize       int32
		maxPoolSize       int32
		labels            map[string]string
		annotations       map[string]string
		usage             *intstr.IntOrString
		expectMinPoolSize int32
		expectMaxPoolSize int32
		expectUsage       *intstr.IntOrString
		expectLabels      map[string]string
		expectAnnotations map[string]string
	}{
		{
			name:              "all empty",
			expectMinPoolSize: consts.DefaultMinPoolSize,
			expectMaxPoolSize: consts.DefaultMinPoolSize * consts.DefaultMaxPoolSizeFactor,
			expectUsage:       ptr.To(intstr.Parse("50%")),
			expectLabels: map[string]string{
				consts.LabelSandboxPool:  "base",
				consts.LabelTemplateHash: "b5454c8d5",
			},
			expectAnnotations: map[string]string{},
		},
		{
			name: "remove inner prefix",
			labels: map[string]string{
				"foo":                         "bar",
				consts.InternalPrefix + "foo": "bar",
			},
			annotations: map[string]string{
				"foo":                         "bar",
				consts.InternalPrefix + "foo": "bar",
			},
			expectMinPoolSize: consts.DefaultMinPoolSize,
			expectMaxPoolSize: consts.DefaultMinPoolSize * consts.DefaultMaxPoolSizeFactor,
			expectUsage:       ptr.To(intstr.Parse("50%")),
			expectLabels: map[string]string{
				"foo":                    "bar",
				consts.LabelSandboxPool:  "base",
				consts.LabelTemplateHash: "6d87bdfc99",
			},
			expectAnnotations: map[string]string{
				"foo": "bar",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := &SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      tt.labels,
					Annotations: tt.annotations,
				},
				Spec: SandboxTemplateSpec{
					MinPoolSize: tt.minPoolSize,
					MaxPoolSize: tt.maxPoolSize,
					ExpectUsage: tt.usage,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:        "test-pod",
							Labels:      tt.labels,
							Annotations: tt.annotations,
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "test-image",
								},
							},
						},
					},
				},
			}
			template.Init("default")
			expect := &SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "base",
					Namespace:   "default",
					Labels:      tt.expectLabels,
					Annotations: tt.expectAnnotations,
				},
				Spec: SandboxTemplateSpec{
					MinPoolSize: consts.DefaultMinPoolSize,
					MaxPoolSize: consts.DefaultMinPoolSize * consts.DefaultMaxPoolSizeFactor,
					ExpectUsage: tt.expectUsage,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:        "test-pod",
							Labels:      tt.expectLabels,
							Annotations: tt.expectAnnotations,
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "test-image",
								},
							},
						},
					},
				},
			}
			assert.Equal(t, "<no diffs>", diff.ObjectReflectDiff(expect, template))
		})
	}
}

func TestLoadBuiltinTemplates(t *testing.T) {
	wd, err := os.Getwd()
	assert.NoError(t, err)
	tests := []struct {
		name        string
		templateDir string
		expectError bool
	}{
		{
			name:        "load builtin templates",
			templateDir: filepath.Join(wd, "../../../../assets/template/builtin_templates"),
			expectError: false,
		},
		{
			name:        "load from non-existent directory",
			templateDir: "non-existent-directory",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			// 创建 Infra 实例
			infraInstance := &FakeInfra{
				BaseInfra: BaseInfra{
					Namespace:   "default",
					TemplateDir: tt.templateDir,
				},
			}

			ctx := context.Background()
			err = LoadBuiltinTemplates(ctx, infraInstance, tt.templateDir, "default")

			if tt.expectError {
				assert.Error(t, err)
				return
			} else {
				assert.NoError(t, err)

				info := infraInstance.LoadDebugInfo()

				// 验证 code-interpreter 模板是否被成功加载
				_, ok := info["code-interpreter"]
				assert.True(t, ok, "code-interpreter template should be loaded")

				// 验证 browser 模板是否被成功加载
				_, ok = info["browser"]
				assert.True(t, ok, "browser template should be loaded")

				// 验证 desktop 模板是否被成功加载
				_, ok = info["desktop"]
				assert.True(t, ok, "desktop template should be loaded")
			}
		})
	}
}
