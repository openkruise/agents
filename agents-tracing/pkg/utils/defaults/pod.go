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

package defaults

import (
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/kubernetes/pkg/apis/core/v1"
)

// SetDefaultPodSpec sets default pod spec
func SetDefaultPodSpec(in *corev1.PodSpec) {
	v1.SetDefaults_PodSpec(in)
	// default pod volumes
	setDefaultPodVolumes(in.Volumes)

	setDefaultInitContainers(in.InitContainers)
	setDefaultContainers(in.Containers)
	setDefaultEphemeralContainers(in.EphemeralContainers)

	v1.SetDefaults_ResourceList(&in.Overhead)
}

func setDefaultInitContainers(containers []corev1.Container) {
	for i := range containers {
		container := &containers[i]
		v1.SetDefaults_Container(container)
		setDefaultContainerPorts(container)
		setDefaultContainerEnv(container)
		v1.SetDefaults_ResourceList(&container.Resources.Limits)
		v1.SetDefaults_ResourceList(&container.Resources.Requests)
		setDefaultContainerProbes(container)
		setDefaultContainerLifecycle(container)
	}
}

func setDefaultContainers(containers []corev1.Container) {
	for i := range containers {
		container := &containers[i]
		v1.SetDefaults_Container(container)
		setDefaultContainerPorts(container)
		setDefaultContainerEnv(container)
		v1.SetDefaults_ResourceList(&container.Resources.Limits)
		v1.SetDefaults_ResourceList(&container.Resources.Requests)
		setDefaultContainerProbes(container)
		setDefaultContainerLifecycle(container)
	}
}
func setDefaultEphemeralContainers(containers []corev1.EphemeralContainer) {
	for i := range containers {
		container := &containers[i]
		setDefaultEphemeralContainerPorts(container)
		setDefaultEphemeralContainerEnv(container)
		v1.SetDefaults_ResourceList(&container.EphemeralContainerCommon.Resources.Limits)
		v1.SetDefaults_ResourceList(&container.EphemeralContainerCommon.Resources.Requests)
		setDefaultEphemeralContainerProbes(container)
		setDefaultEphemeralContainerLifecycle(container)
	}
}

func setDefaultContainerPorts(container *corev1.Container) {
	for j := range container.Ports {
		port := &container.Ports[j]
		if port.Protocol == "" {
			port.Protocol = corev1.ProtocolTCP
		}
	}
}

func setDefaultContainerEnv(container *corev1.Container) {
	for j := range container.Env {
		env := &container.Env[j]
		if env.ValueFrom != nil && env.ValueFrom.FieldRef != nil {
			v1.SetDefaults_ObjectFieldSelector(env.ValueFrom.FieldRef)
		}
	}
}

func setDefaultContainerProbes(container *corev1.Container) {
	if container.LivenessProbe != nil {
		v1.SetDefaults_Probe(container.LivenessProbe)
		if container.LivenessProbe.ProbeHandler.HTTPGet != nil {
			v1.SetDefaults_HTTPGetAction(container.LivenessProbe.ProbeHandler.HTTPGet)
		}
	}
	if container.ReadinessProbe != nil {
		v1.SetDefaults_Probe(container.ReadinessProbe)
		if container.ReadinessProbe.ProbeHandler.HTTPGet != nil {
			v1.SetDefaults_HTTPGetAction(container.ReadinessProbe.ProbeHandler.HTTPGet)
		}
	}
	if container.StartupProbe != nil {
		v1.SetDefaults_Probe(container.StartupProbe)
		if container.StartupProbe.ProbeHandler.HTTPGet != nil {
			v1.SetDefaults_HTTPGetAction(container.StartupProbe.ProbeHandler.HTTPGet)
		}
	}
}

func setDefaultContainerLifecycle(container *corev1.Container) {
	if container.Lifecycle != nil {
		if container.Lifecycle.PostStart != nil && container.Lifecycle.PostStart.HTTPGet != nil {
			v1.SetDefaults_HTTPGetAction(container.Lifecycle.PostStart.HTTPGet)
		}
		if container.Lifecycle.PreStop != nil && container.Lifecycle.PreStop.HTTPGet != nil {
			v1.SetDefaults_HTTPGetAction(container.Lifecycle.PreStop.HTTPGet)
		}
	}
}

func setDefaultEphemeralContainerPorts(container *corev1.EphemeralContainer) {
	for j := range container.EphemeralContainerCommon.Ports {
		port := &container.EphemeralContainerCommon.Ports[j]
		if port.Protocol == "" {
			port.Protocol = corev1.ProtocolTCP
		}
	}
}

func setDefaultEphemeralContainerEnv(container *corev1.EphemeralContainer) {
	for j := range container.EphemeralContainerCommon.Env {
		env := &container.EphemeralContainerCommon.Env[j]
		if env.ValueFrom != nil && env.ValueFrom.FieldRef != nil {
			v1.SetDefaults_ObjectFieldSelector(env.ValueFrom.FieldRef)
		}
	}
}

func setDefaultEphemeralContainerProbes(container *corev1.EphemeralContainer) {
	if container.EphemeralContainerCommon.LivenessProbe != nil {
		v1.SetDefaults_Probe(container.EphemeralContainerCommon.LivenessProbe)
		if container.EphemeralContainerCommon.LivenessProbe.ProbeHandler.HTTPGet != nil {
			v1.SetDefaults_HTTPGetAction(container.EphemeralContainerCommon.LivenessProbe.ProbeHandler.HTTPGet)
		}
	}
	if container.EphemeralContainerCommon.ReadinessProbe != nil {
		v1.SetDefaults_Probe(container.EphemeralContainerCommon.ReadinessProbe)
		if container.EphemeralContainerCommon.ReadinessProbe.ProbeHandler.HTTPGet != nil {
			v1.SetDefaults_HTTPGetAction(container.EphemeralContainerCommon.ReadinessProbe.ProbeHandler.HTTPGet)
		}
	}
	if container.EphemeralContainerCommon.StartupProbe != nil {
		v1.SetDefaults_Probe(container.EphemeralContainerCommon.StartupProbe)
		if container.EphemeralContainerCommon.StartupProbe.ProbeHandler.HTTPGet != nil {
			v1.SetDefaults_HTTPGetAction(container.EphemeralContainerCommon.StartupProbe.ProbeHandler.HTTPGet)
		}
	}
}

func setDefaultEphemeralContainerLifecycle(container *corev1.EphemeralContainer) {
	if container.EphemeralContainerCommon.Lifecycle != nil {
		if container.EphemeralContainerCommon.Lifecycle.PostStart != nil &&
			container.EphemeralContainerCommon.Lifecycle.PostStart.HTTPGet != nil {
			v1.SetDefaults_HTTPGetAction(container.EphemeralContainerCommon.Lifecycle.PostStart.HTTPGet)
		}
		if container.EphemeralContainerCommon.Lifecycle.PreStop != nil &&
			container.EphemeralContainerCommon.Lifecycle.PreStop.HTTPGet != nil {
			v1.SetDefaults_HTTPGetAction(container.EphemeralContainerCommon.Lifecycle.PreStop.HTTPGet)
		}
	}
}

func setDefaultPodVolumes(volumes []corev1.Volume) {
	for i := range volumes {
		volume := &volumes[i]
		v1.SetDefaults_Volume(volume)
		if volume.VolumeSource.HostPath != nil {
			v1.SetDefaults_HostPathVolumeSource(volume.VolumeSource.HostPath)
		}
		if volume.VolumeSource.Secret != nil {
			v1.SetDefaults_SecretVolumeSource(volume.VolumeSource.Secret)
		}
		if volume.VolumeSource.DownwardAPI != nil {
			v1.SetDefaults_DownwardAPIVolumeSource(volume.VolumeSource.DownwardAPI)
			for j := range volume.VolumeSource.DownwardAPI.Items {
				item := &volume.VolumeSource.DownwardAPI.Items[j]
				if item.FieldRef != nil {
					v1.SetDefaults_ObjectFieldSelector(item.FieldRef)
				}
			}
		}
		if volume.VolumeSource.ConfigMap != nil {
			v1.SetDefaults_ConfigMapVolumeSource(volume.VolumeSource.ConfigMap)
		}
		if volume.VolumeSource.Projected != nil {
			v1.SetDefaults_ProjectedVolumeSource(volume.VolumeSource.Projected)
			for j := range volume.VolumeSource.Projected.Sources {
				source := &volume.VolumeSource.Projected.Sources[j]
				if source.DownwardAPI != nil {
					for k := range source.DownwardAPI.Items {
						item := &source.DownwardAPI.Items[k]
						if item.FieldRef != nil {
							v1.SetDefaults_ObjectFieldSelector(item.FieldRef)
						}
					}
				}
				if source.ServiceAccountToken != nil {
					v1.SetDefaults_ServiceAccountTokenProjection(source.ServiceAccountToken)
				}
			}
		}
	}
}
