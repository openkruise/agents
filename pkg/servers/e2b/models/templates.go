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

package models

import (
	"encoding/json"
	"time"
)

// Template represents an E2B template
type Template struct {
	TemplateID    string     `json:"templateID"`
	Public        bool       `json:"public"`
	Aliases       []string   `json:"aliases"`
	Names         []string   `json:"names"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	LastSpawnedAt *time.Time `json:"lastSpawnedAt"`
	SpawnCount    int64      `json:"spawnCount"`
	Builds        []Build    `json:"builds"`
}

// TemplateInfo represents simplified template information for list response
type TemplateInfo struct {
	TemplateID    string     `json:"templateID"`
	BuildID       string     `json:"buildID"`
	CPUCount      int        `json:"cpuCount"`
	MemoryMB      int        `json:"memoryMB"`
	DiskSizeMB    int        `json:"diskSizeMB"`
	Public        bool       `json:"public"`
	Aliases       []string   `json:"aliases"`
	Names         []string   `json:"names"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	CreatedBy     *TeamUser  `json:"createdBy"`
	LastSpawnedAt *time.Time `json:"lastSpawnedAt"`
	SpawnCount    int64      `json:"spawnCount"`
	BuildCount    int        `json:"buildCount"`
	EnvdVersion   string     `json:"envdVersion"`
	BuildStatus   string     `json:"buildStatus"`
}

// Build represents a build of a template
type Build struct {
	BuildID     string    `json:"buildID"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	CPUCount    int       `json:"cpuCount"`
	MemoryMB    int       `json:"memoryMB"`
	FinishedAt  time.Time `json:"finishedAt"`
	DiskSizeMB  int       `json:"diskSizeMB"`
	EnvdVersion string    `json:"envdVersion"`
}

// TemplateBuildRequestV3 is the reserve request of the E2B v3 Template Build
// protocol. Pointer fields preserve the distinction between an omitted value
// and an explicit zero, which the reserve handler validates differently.
type TemplateBuildRequestV3 struct {
	Name          string   `json:"name,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Alias         string   `json:"alias,omitempty"`
	TeamID        string   `json:"teamID,omitempty"`
	CPUCount      *int     `json:"cpuCount,omitempty"`
	MemoryMB      *int     `json:"memoryMB,omitempty"`
	MinFreeDiskMB *int     `json:"minFreeDiskMb,omitempty"`
}

// TemplateRequestResponseV3 is the accepted reservation returned by POST
// /v3/templates.
type TemplateRequestResponseV3 struct {
	TemplateID string   `json:"templateID"`
	BuildID    string   `json:"buildID"`
	Public     bool     `json:"public"`
	Names      []string `json:"names"`
	Tags       []string `json:"tags"`
	Aliases    []string `json:"aliases"`
}

// TemplateStep is one instruction serialized by the E2B Template DSL.
type TemplateStep struct {
	Type  string   `json:"type"`
	Args  []string `json:"args,omitempty"`
	Force bool     `json:"force,omitempty"`
}

// TemplateBuildStartV2 is the E2B v2 build-submission request. FromImage and
// FromTemplate are pointers because the protocol distinguishes an omitted
// field from an explicitly empty one and requires exactly one non-empty base.
type TemplateBuildStartV2 struct {
	FromImage         *string         `json:"fromImage,omitempty"`
	FromTemplate      *string         `json:"fromTemplate,omitempty"`
	FromImageRegistry json.RawMessage `json:"fromImageRegistry,omitempty"`
	Force             bool            `json:"force,omitempty"`
	Steps             []TemplateStep  `json:"steps,omitempty"`
	StartCmd          *string         `json:"startCmd,omitempty"`
	ReadyCmd          *string         `json:"readyCmd,omitempty"`
}

// TemplateBuildInfo is the E2B build-status response.
type TemplateBuildInfo struct {
	Logs       []string                 `json:"logs"`
	LogEntries []TemplateBuildLogEntry  `json:"logEntries"`
	TemplateID string                   `json:"templateID"`
	BuildID    string                   `json:"buildID"`
	Status     string                   `json:"status"`
	Reason     *TemplateBuildFailReason `json:"reason,omitempty"`
}

// TemplateBuildLogEntry is one persisted Build log entry.
type TemplateBuildLogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
	Level     string    `json:"level"`
	Step      string    `json:"step,omitempty"`
}

// TemplateBuildFailReason explains an errored Build.
type TemplateBuildFailReason struct {
	Message    string                  `json:"message"`
	Step       string                  `json:"step,omitempty"`
	LogEntries []TemplateBuildLogEntry `json:"logEntries,omitempty"`
}

// TemplateAliasResponse answers the existence query used by Template.exists.
type TemplateAliasResponse struct {
	TemplateID string `json:"templateID"`
	Public     bool   `json:"public"`
}
