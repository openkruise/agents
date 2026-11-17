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
//	Args:    ["--", <original command...>, <original args...>]
//
// A main container without an explicit command (image entrypoint) is wrapped
// with an empty template argv. Such a pool is admitted, but every claim must
// supply spec.command; otherwise the claim fails because the image entrypoint
// is unrecoverable once the command is replaced.
func wrapMainContainerWithRunWithEnvs(box *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
	if box == nil || pod == nil {
		return
	}
	if box.Spec.StartupMode != agentsv1alpha1.StartupModeDeferred {
		return
	}
	if len(pod.Spec.Containers) == 0 {
		klog.InfoS("skip env-var wrapping: sandbox pod has no containers", "sandbox", klog.KObj(box))
		return
	}

	main := &pod.Spec.Containers[0]
	if isAlreadyWrapped(main.Command) {
		return
	}

	original := make([]string, 0, len(main.Command)+len(main.Args))
	original = append(original, main.Command...)
	original = append(original, main.Args...)
	main.Command = []string{"/bin/bash", utils.RunWithEnvsScript}
	main.Args = append([]string{"--"}, original...)

	// The wrapper contract: claim directory + pod UID generation check.
	appendEnvIfMissing(main, corev1.EnvVar{Name: utils.EnvSandboxEnvDir, Value: utils.SandboxEnvClaimDir})
	appendEnvIfMissing(main, corev1.EnvVar{
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

	klog.InfoS("wrapped main container with run_with_envs.sh",
		"sandbox", klog.KObj(box), "container", main.Name, "script", utils.RunWithEnvsScript)
}

// isAlreadyWrapped reports whether the container command already points at the
// wrapper, so a pod generation path that wraps twice cannot double-wrap.
func isAlreadyWrapped(command []string) bool {
	return len(command) >= 2 && command[1] == utils.RunWithEnvsScript
}

// wrapExecProbes rewrites every exec probe of the container to the wrapper's
// probe mode; non-exec probes are left as-is.
func wrapExecProbes(c *corev1.Container) {
	probes := []*corev1.Probe{c.StartupProbe, c.ReadinessProbe, c.LivenessProbe}
	for _, p := range probes {
		if p == nil || p.Exec == nil || len(p.Exec.Command) == 0 {
			continue
		}
		if len(p.Exec.Command) >= 3 && p.Exec.Command[2] == "--probe" {
			continue
		}
		wrapped := make([]string, 0, len(p.Exec.Command)+4)
		wrapped = append(wrapped, "/bin/bash", utils.RunWithEnvsScript, "--probe", "--")
		wrapped = append(wrapped, p.Exec.Command...)
		p.Exec.Command = wrapped
	}
}

func appendEnvIfMissing(c *corev1.Container, env corev1.EnvVar) {
	for _, e := range c.Env {
		if e.Name == env.Name {
			return
		}
	}
	c.Env = append(c.Env, env)
}
