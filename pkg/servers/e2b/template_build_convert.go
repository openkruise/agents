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

package e2b

import (
	"bytes"
	"fmt"
	"net/http"
	"path"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

// convertedBuild is the protocol-neutral recipe submitted to Manager. Exactly
// one of Image and BuildOptions.FromTemplate is non-empty. The E2B entry always
// requests a checkpoint, so the conversion materializes that policy once in
// the shared persisted options.
type convertedBuild struct {
	Image        string
	BuildOptions agentsv1alpha1.SandboxTemplateBuildOptions
}

// buildCursor is the effective WORKDIR, USER, and ENV context of the E2B DSL.
// Every generated command receives a private snapshot of this context.
type buildCursor struct {
	user string
	cwd  string
	env  map[string]string
}

func badRequest(format string, a ...any) *web.ApiError {
	return &web.ApiError{Code: http.StatusBadRequest, Message: fmt.Sprintf(format, a...)}
}

// convertBuildRecipe folds the E2B Template DSL into self-contained commands.
// The API layer owns this protocol conversion; Manager receives neither E2B
// wire models nor implicit WORKDIR, USER, or ENV cursor state.
func convertBuildRecipe(req models.TemplateBuildStartV2) (*convertedBuild, *web.ApiError) {
	// A JSON null marshals to the literal bytes "null", which semantically
	// means absent for SDK bindings that serialize every field.
	if len(req.FromImageRegistry) != 0 && !bytes.Equal(bytes.TrimSpace(req.FromImageRegistry), []byte("null")) {
		return nil, badRequest("fromImageRegistry is not supported")
	}

	image, fromTemplate, apiErr := convertBuildBase(req.FromImage, req.FromTemplate)
	if apiErr != nil {
		return nil, apiErr
	}

	converted := &convertedBuild{
		Image: image,
		BuildOptions: agentsv1alpha1.SandboxTemplateBuildOptions{
			FromTemplate: fromTemplate,
			Checkpoint:   true,
		},
	}
	cursor := &buildCursor{env: map[string]string{}}
	for i, step := range req.Steps {
		switch step.Type {
		case "RUN":
			if len(step.Args) < 1 || strings.TrimSpace(step.Args[0]) == "" {
				return nil, badRequest("steps[%d].args must carry a non-empty command", i)
			}
			if len(step.Args) > 2 {
				return nil, badRequest("steps[%d].args accepts at most [command, user]", i)
			}
			user := cursor.user
			if len(step.Args) == 2 {
				if strings.TrimSpace(step.Args[1]) == "" {
					return nil, badRequest("steps[%d].args user must not be empty", i)
				}
				user = step.Args[1]
			}
			converted.BuildOptions.Steps = append(converted.BuildOptions.Steps, cursor.command(step.Args[0], user))
		case "WORKDIR":
			if len(step.Args) != 1 || !path.IsAbs(step.Args[0]) {
				return nil, badRequest("steps[%d].args must carry one absolute path", i)
			}
			cursor.cwd = step.Args[0]
		case "USER":
			if len(step.Args) != 1 || strings.TrimSpace(step.Args[0]) == "" {
				return nil, badRequest("steps[%d].args must carry a non-empty user", i)
			}
			cursor.user = step.Args[0]
		case "ENV":
			if len(step.Args)%2 != 0 {
				return nil, badRequest("steps[%d].args must be key/value pairs", i)
			}
			for j := 0; j < len(step.Args); j += 2 {
				cursor.env[step.Args[j]] = step.Args[j+1]
			}
		default:
			return nil, badRequest("steps[%d].type %q is not supported", i, step.Type)
		}
	}

	if converted.BuildOptions.StartCommand, apiErr = convertLifecycleCommand("startCmd", req.StartCmd, cursor); apiErr != nil {
		return nil, apiErr
	}
	if converted.BuildOptions.ReadyCommand, apiErr = convertLifecycleCommand("readyCmd", req.ReadyCmd, cursor); apiErr != nil {
		return nil, apiErr
	}
	return converted, nil
}

func convertBuildBase(fromImage, fromTemplate *string) (string, string, *web.ApiError) {
	if fromImage != nil && strings.TrimSpace(*fromImage) == "" {
		return "", "", badRequest("fromImage must not be empty")
	}
	if fromTemplate != nil && strings.TrimSpace(*fromTemplate) == "" {
		return "", "", badRequest("fromTemplate must not be empty")
	}
	if fromImage != nil && fromTemplate != nil {
		return "", "", badRequest("fromImage and fromTemplate are mutually exclusive")
	}
	if fromImage == nil && fromTemplate == nil {
		return "", "", badRequest("exactly one of fromImage or fromTemplate is required")
	}
	if fromImage != nil {
		return *fromImage, "", nil
	}
	return "", *fromTemplate, nil
}

func convertLifecycleCommand(field string, command *string, cursor *buildCursor) (*agentsv1alpha1.TemplateBuildCommand, *web.ApiError) {
	if command == nil {
		return nil, nil
	}
	if strings.TrimSpace(*command) == "" {
		return nil, badRequest("%s must not be empty", field)
	}
	folded := cursor.command(*command, cursor.user)
	return &folded, nil
}

func (c *buildCursor) command(command, user string) agentsv1alpha1.TemplateBuildCommand {
	folded := agentsv1alpha1.TemplateBuildCommand{
		Command: command,
		User:    user,
		Cwd:     c.cwd,
	}
	if len(c.env) > 0 {
		folded.Env = make(map[string]string, len(c.env))
		for key, value := range c.env {
			folded.Env[key] = value
		}
	}
	return folded
}
