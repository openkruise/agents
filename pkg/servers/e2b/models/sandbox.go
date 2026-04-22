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

// Package models provides data models for the E2B sandbox API.
package models

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/openkruise/agents/api/v1alpha1"
)

const (
	SandboxStateRunning = "running"
	SandboxStatePaused  = "paused"
)

// Sandbox represents an E2B sandbox running as a Kubernetes Pod
type Sandbox struct {
	TemplateID      string            `json:"templateID"`
	SandboxID       string            `json:"sandboxID"`
	ClientID        string            `json:"clientID"`
	StartedAt       string            `json:"startedAt"`
	EndAt           string            `json:"endAt"`
	EnvdVersion     string            `json:"envdVersion"`
	EnvdAccessToken string            `json:"envdAccessToken,omitempty"`
	Domain          string            `json:"domain"`
	CPUCount        int64             `json:"cpuCount"`
	MemoryMB        int64             `json:"memoryMB"`
	DiskSizeMB      int64             `json:"diskSizeMB"`
	Alias           string            `json:"alias"`
	Metadata        map[string]string `json:"metadata"`
	State           string            `json:"state"`
}

// NewSandboxRequest represents a request to create a new sandbox
type NewSandboxRequest struct {
	TemplateID string            `json:"templateID"`
	Timeout    int               `json:"timeout,omitempty"`
	AutoPause  bool              `json:"autoPause,omitempty"`
	Secure     bool              `json:"secure,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	EnvVars    EnvVars           `json:"envVars,omitempty"`

	Extensions NewSandboxRequestExtension `json:"-"`
}

type NewSandboxRequestExtension struct {
	InplaceUpdate        InplaceUpdateExtension
	CSIMount             CSIMountExtension
	SkipInitRuntime      bool
	ReserveFailedSandbox bool
	CreateOnNoStock      bool
	WaitReadySeconds     int
	TimeoutSeconds       int
	NeverTimeout         bool
	Labels               map[string]string
}

type InplaceUpdateExtension struct {
	Image     string
	Resources *InplaceUpdateResourcesExtension
}

type InplaceUpdateResourcesExtension struct {
	Requests corev1.ResourceList
	Limits   corev1.ResourceList
}

type CSIMountExtension struct {
	MountConfigs []v1alpha1.CSIMountConfig `json:"mountConfigs"` // list of CSI mount configurations
}

// SandboxMetadata represents metadata for a sandbox
type SandboxMetadata map[string]string

// EnvVars represents environment variables for a sandbox
type EnvVars map[string]string

type SetTimeoutRequest struct {
	TimeoutSeconds int `json:"timeout"`
}

type NewSnapshotRequest struct {
	Name       string                      `json:"name"` // name is not used by the E2B SDK yet, just reserved for future use
	Extensions NewSnapshotRequestExtension `json:"-"`
}

type NewSnapshotRequestExtension struct {
	KeepRunning        *bool
	TTL                *string
	PersistentContents []string
	WaitSuccessSeconds int
}

const (
	// CDPPort is the port used for CDP (Chrome DevTools Port) communication
	CDPPort = 9222
)
