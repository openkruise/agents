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

package sandboxcr

import (
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// This mode starts a new process; memory restore and pod-info-only checkpoints
// cannot satisfy its filesystem recovery contract.
func validateStartupCheckpoint(cp *v1alpha1.Checkpoint, user string) error {
	if user == "" || cp.Annotations[v1alpha1.AnnotationOwner] != user {
		return managererrors.NewError(managererrors.ErrorNotFound, "checkpoint not found")
	}
	if cp.DeletionTimestamp != nil || cp.Status.Phase != v1alpha1.CheckpointSucceeded || cp.Status.CheckpointId == "" {
		return managererrors.NewError(managererrors.ErrorBadRequest, "checkpoint is not ready")
	}
	if !slices.Contains(cp.Spec.PersistentContents, v1alpha1.CheckpointPersistentContentFilesystem) ||
		slices.Contains(cp.Spec.PersistentContents, v1alpha1.CheckpointPersistentContentMemory) {
		return managererrors.NewError(managererrors.ErrorBadRequest, "new workload requires a filesystem checkpoint without memory restore")
	}
	return nil
}

// applyCloneStartup operates only on the deep copy that will become the new
// sandbox, before quota admission and create. It never updates shared templates.
func applyCloneStartup(sbx *v1alpha1.Sandbox, input *infra.CloneStartupOptions) error {
	if sbx.Spec.Template == nil || len(sbx.Spec.Template.Spec.Containers) != 1 {
		return managererrors.NewError(managererrors.ErrorBadRequest, "new workload requires a single-container checkpoint template")
	}
	if len(input.Command) == 0 || input.InitRuntime.AccessToken == "" {
		return managererrors.NewError(managererrors.ErrorBadRequest, "new workload requires command and runtime credentials")
	}
	container := &sbx.Spec.Template.Spec.Containers[0]
	container.Command = slices.Clone(input.Command)
	container.Args = nil
	for _, name := range slices.Sorted(maps.Keys(input.EnvVars)) {
		value := corev1.EnvVar{Name: name, Value: input.EnvVars[name]}
		index := slices.IndexFunc(container.Env, func(env corev1.EnvVar) bool { return env.Name == name })
		if index < 0 {
			container.Env = append(container.Env, value)
		} else {
			container.Env[index] = value
		}
	}
	var err error
	if input.ResourceRequests != nil {
		container.Resources.Requests, err = coldResources(input.ResourceRequests)
		if err != nil {
			return err
		}
	}
	if input.ResourceLimits != nil {
		container.Resources.Limits, err = coldResources(input.ResourceLimits)
		if err != nil {
			return err
		}
	}
	if input.OS != "" || input.Architecture != "" {
		if sbx.Spec.Template.Spec.NodeSelector == nil {
			sbx.Spec.Template.Spec.NodeSelector = map[string]string{}
		}
		if input.OS != "" {
			sbx.Spec.Template.Spec.NodeSelector[corev1.LabelOSStable] = input.OS
		}
		if input.Architecture != "" {
			sbx.Spec.Template.Spec.NodeSelector[corev1.LabelArchStable] = input.Architecture
		}
	}
	if !slices.ContainsFunc(sbx.Spec.Runtimes, func(r v1alpha1.RuntimeConfig) bool {
		return r.Name == v1alpha1.RuntimeConfigForInjectAgentRuntime
	}) {
		sbx.Spec.Runtimes = append(sbx.Spec.Runtimes, v1alpha1.RuntimeConfig{Name: v1alpha1.RuntimeConfigForInjectAgentRuntime})
	}
	return nil
}
