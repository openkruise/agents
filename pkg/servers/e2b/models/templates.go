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

import "time"

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
