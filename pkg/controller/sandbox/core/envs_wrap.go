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

package core

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// wrapMainContainerWithRunWithEnvs rewrites the sandbox main container and its
// exec probes to go through the run_with_envs.sh wrapper when the sandbox
// declares spec.startupMode=Deferred. The wrapper becomes PID 1, holds the main
// process until the claim-time env file, optional claim command, and readiness
// signal are delivered, then sources them and exec's the template or claim
// command — the only way to give the main process claim-time variables (a
// process environment cannot be changed after container start) and to defer
// the main process start until the sandbox is claimed.
//
// It is a no-op unless startupMode is Deferred. The command rewrite mirrors the
// "--" convention used by envd-run.sh:
//
//	Command: ["/bin/bash", "/mnt/envd/run_with_envs.sh"]
//	Args:    ["--", <template command...>, <template args...>]
//
// A main container without an explicit command is wrapped with an empty
// template argv, even when it sets args: those args are meant for the image
// entrypoint, which is unrecoverable once the command is replaced. Such a pool
// is admitted, but every claim must supply spec.command.
//
// The function is idempotent: an already wrapped command is kept, while the
// wrapper env and probe wrapping are still applied.
func wrapMainContainerWithRunWithEnvs(ctx context.Context, box *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
	if box == nil || pod == nil || box.Spec.StartupMode != agentsv1alpha1.StartupModeDeferred {
		return
	}
	if len(pod.Spec.Containers) == 0 {
		return
	}

	main := &pod.Spec.Containers[0]
	if !isWrappedCommand(main.Command) {
		var templateArgv []string
		if len(main.Command) > 0 {
			templateArgv = append(append(templateArgv, main.Command...), main.Args...)
		}
		main.Command = []string{"/bin/bash", utils.RunWithEnvsScript}
		main.Args = append([]string{"--"}, templateArgv...)
	}

	// The wrapper contract: claim directory + pod UID generation check. Both
	// are controller-owned, so template values are overwritten: a different
	// SANDBOX_POD_UID never matches the readiness signal, and a different
	// SANDBOX_ENV_DIR is never written by the delivery path.
	setContainerEnv(main, corev1.EnvVar{Name: utils.EnvSandboxEnvDir, Value: utils.SandboxEnvClaimDir})
	setContainerEnv(main, corev1.EnvVar{
		Name: utils.EnvSandboxPodUID,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  "metadata.uid",
			},
		},
	})

	// exec probes run through the wrapper too: before the readiness signal
	// they succeed immediately (the pre-warmed pod must stay healthy), after
	// it they run the real probe command with the claim env sourced.
	wrapExecProbes(main)

	klog.FromContext(ctx).V(utils.DebugLogLevel).Info("wrapped main container with run_with_envs.sh",
		"sandbox", klog.KObj(box), "container", main.Name)
}

// isWrappedCommand reports whether a command already runs through the wrapper.
func isWrappedCommand(command []string) bool {
	return len(command) >= 2 && command[1] == utils.RunWithEnvsScript
}

// isWrappedProbe reports whether an exec probe command already runs through
// the wrapper in probe mode.
func isWrappedProbe(command []string) bool {
	return isWrappedCommand(command) && len(command) >= 3 && command[2] == "--probe"
}

// wrapExecProbes rewrites every exec probe of the container to the wrapper's
// probe mode; non-exec probes are left as-is.
func wrapExecProbes(c *corev1.Container) {
	for _, p := range []*corev1.Probe{c.StartupProbe, c.ReadinessProbe, c.LivenessProbe} {
		if p == nil || p.Exec == nil || len(p.Exec.Command) == 0 || isWrappedProbe(p.Exec.Command) {
			continue
		}
		p.Exec.Command = append([]string{"/bin/bash", utils.RunWithEnvsScript, "--probe", "--"}, p.Exec.Command...)
	}
}

// setContainerEnv sets env on the container, replacing every existing entry
// with the same name.
func setContainerEnv(c *corev1.Container, env corev1.EnvVar) {
	found := false
	for i := range c.Env {
		if c.Env[i].Name == env.Name {
			c.Env[i] = env
			found = true
		}
	}
	if !found {
		c.Env = append(c.Env, env)
	}
}
