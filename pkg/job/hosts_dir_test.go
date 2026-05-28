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

package job

import (
	"strings"
	"testing"
)

func TestExtractRegistryHost(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "registry with tag",
			image: "registry.example.com/repo:tag",
			want:  "registry.example.com",
		},
		{
			name:  "registry with port and tag",
			image: "registry.example.com:5000/repo:tag",
			want:  "registry.example.com:5000",
		},
		{
			name:  "registry with org and tag",
			image: "registry.example.com/org/repo:v1.0",
			want:  "registry.example.com",
		},
		{
			name:  "registry with port, org, and tag",
			image: "registry.example.com:5000/org/repo:v1.0",
			want:  "registry.example.com:5000",
		},
		{
			name:  "registry with dot in host",
			image: "my-registry.io/myimage:latest",
			want:  "my-registry.io",
		},
		{
			name:  "localhost without port",
			image: "localhost/myimage:latest",
			want:  "localhost",
		},
		{
			name:  "localhost with port",
			image: "localhost:5000/myimage:latest",
			want:  "localhost:5000",
		},
		{
			name:  "docker hub image with tag",
			image: "ubuntu:latest",
			want:  defaultRegistryHost,
		},
		{
			name:  "docker hub library image",
			image: "library/nginx",
			want:  defaultRegistryHost,
		},
		{
			name:  "docker hub library image with tag",
			image: "library/nginx:1.25",
			want:  defaultRegistryHost,
		},
		{
			name:  "docker hub user image with tag",
			image: "myuser/myrepo:v1",
			want:  defaultRegistryHost,
		},
		{
			name:  "registry with digest",
			image: "registry.example.com/repo@sha256:abc123",
			want:  "registry.example.com",
		},
		{
			name:  "docker hub with digest",
			image: "ubuntu@sha256:abc123",
			want:  defaultRegistryHost,
		},
		{
			name:  "registry without tag",
			image: "registry.example.com/repo",
			want:  "registry.example.com",
		},
		{
			name:  "bare image name",
			image: "ubuntu",
			want:  defaultRegistryHost,
		},
		{
			name:  "ACR private registry",
			image: "acr-test.cn-hangzhou.cr.aliyuncs.com/ns/repo:v1",
			want:  "acr-test.cn-hangzhou.cr.aliyuncs.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRegistryHost(tt.image)
			if got != tt.want {
				t.Errorf("extractRegistryHost(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func TestPrepareHostsDirScript(t *testing.T) {
	script := PrepareHostsDirScript()

	checks := []struct {
		name    string
		content string
	}{
		{"source path", "/etc/containerd/certs.d.orig"},
		{"dest path", "/etc/containerd/certs.d"},
		{"registry env", "$TARGET_REGISTRY"},
		{"copy command", "cp -a"},
		{"push capability", `"push"`},
		{"hosts.toml file", "hosts.toml"},
		{"capabilities key", "capabilities"},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(script, c.content) {
				t.Errorf("PrepareHostsDirScript() missing expected content: %q", c.content)
			}
		})
	}
}
