// Package models provides data models for the E2B sandbox API.
package models

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
	EnvdAccessToken string            `json:"envdAccessToken"`
	Domain          string            `json:"domain"`
	CPUCount        int64             `json:"cpuCount"`
	MemoryMB        int64             `json:"memoryMB"`
	DiskSizeMB      int64             `json:"diskSizeMB"`
	Alias           string            `json:"alias"`
	Metadata        map[string]string `json:"metadata"`
	State           string            `json:"state"`
}

func (s *Sandbox) Created() bool {
	return s.State == SandboxStateRunning || s.State == SandboxStatePaused
}

// NewSandboxRequest represents a request to create a new sandbox
type NewSandboxRequest struct {
	TemplateID string            `json:"templateID"`
	Timeout    int               `json:"timeout,omitempty"`
	AutoPause  bool              `json:"autoPause,omitempty"`
	Secure     bool              `json:"secure,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	EnvVars    EnvVars           `json:"envVars,omitempty"`
}

// SandboxMetadata represents metadata for a sandbox
type SandboxMetadata map[string]string

// EnvVars represents environment variables for a sandbox
type EnvVars map[string]string

type SetTimeoutRequest struct {
	TimeoutSeconds int `json:"timeout"`
}

type ListSandboxesRequest struct {
	Metadata  map[string]string `json:"metadata,omitempty"`
	State     string            `json:"state,omitempty"`
	NextToken string            `json:"nextToken,omitempty"`
	Limit     int32             `json:"limit,omitempty"`
}

const (
	// EnvdPort is the port used for envd communication
	EnvdPort = 49983
	// CDPPort is the port used for CDP (Chrome DevTools Port) communication
	CDPPort = 9222
	VNCPort = 6080
)
