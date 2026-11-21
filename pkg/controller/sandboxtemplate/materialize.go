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

package sandboxtemplate

import (
	_ "embed"
	"fmt"
	"path"
	"strings"

	api "github.com/openkruise/agents/api/v1alpha1"
)

//go:embed copy_script.sh
var copyScript string

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// materializeCommand turns one COPY step into the command that runs the
// embedded copy script: five named, shell-quoted assignments (archive,
// targetPath, sourcePrefix, owner, permissions) followed by the static
// script body. User input never enters the script text itself.
func materializeCommand(copy *api.TemplateBuildCopy) (*api.TemplateBuildCommand, error) {
	if len(copy.FilesHash) != 64 || copy.Src == "" || path.IsAbs(copy.Src) || path.Clean(copy.Src) == ".." || strings.HasPrefix(path.Clean(copy.Src), "../") || !path.IsAbs(copy.Dest) {
		return nil, fmt.Errorf("invalid COPY paths or archive key")
	}
	for _, c := range copy.FilesHash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, fmt.Errorf("invalid COPY archive key")
		}
	}
	prefix := copy.Src
	segments := strings.Split(copy.Src, "/")
	for index, segment := range segments {
		if strings.ContainsAny(segment, "*?[") {
			prefix = strings.Join(segments[:index], "/")
			break
		}
	}
	sourcePrefix := ""
	if clean := path.Clean(prefix); clean != "." {
		sourcePrefix = "/" + clean
	}
	owner := copy.User
	if owner == "" {
		owner = buildRootUser
	}
	permissions := ""
	if copy.Mode != nil {
		permissions = fmt.Sprintf("%04o", *copy.Mode)
	}
	command := "archive=" + shellQuote("/blobs/"+copy.FilesHash+".tar.gz") + "\n" +
		"targetPath=" + shellQuote(copy.Dest) + "\n" +
		"sourcePrefix=" + shellQuote(sourcePrefix) + "\n" +
		"owner=" + shellQuote(owner) + "\n" +
		"permissions=" + shellQuote(permissions) + "\n" + copyScript
	return &api.TemplateBuildCommand{Command: command, User: buildRootUser, Cwd: "/"}, nil
}
