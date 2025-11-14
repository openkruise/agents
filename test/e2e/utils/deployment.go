package utils

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func GetE2EMarker() map[string]string {
	return map[string]string{
		"component-e2e": "sandbox-operator",
	}
}

func GetPodTemplate(labels, annotations map[string]string,
	modifiers ...func(*corev1.PodTemplateSpec)) corev1.PodTemplateSpec {
	container := corev1.Container{
		Name:            "main-container",
		Image:           "anolis-registry.cn-zhangjiakou.cr.aliyuncs.com/openanolis/nginx:1.14.1-8.6",
		ImagePullPolicy: corev1.PullIfNotPresent,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("1"),
				corev1.ResourceMemory:           resource.MustParse("1Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("30Gi"),
			},
		},
	}

	// 定义 Pod 模板
	podLabels := GetE2EMarker()
	for k, v := range labels {
		podLabels[k] = v
	}
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      podLabels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{container},
		},
	}
	for _, processor := range modifiers {
		processor(&template)
	}
	return template
}

func GetDeployment(replicas int32, labels, annotations map[string]string,
	modifiers ...func(*corev1.PodTemplateSpec)) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: GetPodTemplate(labels, annotations, modifiers...),
		},
	}
}

func GetStatefulSet(replicas int32, labels, annotations map[string]string,
	modifiers ...func(*corev1.PodTemplateSpec)) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: GetPodTemplate(labels, annotations, modifiers...),
		},
	}
}

func GetCloneSet(replicas int32, labels, annotations map[string]string,
	modifiers ...func(*corev1.PodTemplateSpec)) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps.kruise.io/v1alpha1",
			"kind":       "CloneSet",
			"metadata": map[string]interface{}{
				"name":      "app",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"replicas": replicas,
				"selector": map[string]interface{}{
					"matchLabels": labels,
				},
				"template": GetPodTemplate(labels, annotations, modifiers...),
			},
		},
	}
}
