package filter

import (
	"testing"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name:    "default config",
			cfg:     DefaultConfig(),
			wantErr: false,
		},
		{
			name: "custom sandbox header",
			cfg: &Config{
				SandboxHeaderName: "custom-sandbox-id",
				SandboxPortHeader: "custom-sandbox-port",
				HostHeaderName:    "X-Host",
				DefaultPort:       "8080",
			},
			wantErr: false,
		},
		{
			name:    "empty config",
			cfg:     &Config{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetSandboxHeaderName(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *Config
		wantHeaderName string
	}{
		{
			name:           "empty config uses default",
			cfg:            &Config{},
			wantHeaderName: "e2b-sandbox-id",
		},
		{
			name: "custom sandbox header name",
			cfg: &Config{
				SandboxHeaderName: "custom-sandbox-id",
			},
			wantHeaderName: "custom-sandbox-id",
		},
		{
			name:           "default config",
			cfg:            DefaultConfig(),
			wantHeaderName: "e2b-sandbox-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.GetSandboxHeaderName()
			if got != tt.wantHeaderName {
				t.Errorf("GetSandboxHeaderName() = %q, want %q", got, tt.wantHeaderName)
			}
		})
	}
}

func TestGetHostHeaderName(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *Config
		wantHeaderName string
	}{
		{
			name:           "empty config uses default",
			cfg:            &Config{},
			wantHeaderName: "Host",
		},
		{
			name: "custom host header name",
			cfg: &Config{
				HostHeaderName: "X-Forwarded-Host",
			},
			wantHeaderName: "X-Forwarded-Host",
		},
		{
			name:           "default config",
			cfg:            DefaultConfig(),
			wantHeaderName: "Host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.GetHostHeaderName()
			if got != tt.wantHeaderName {
				t.Errorf("GetHostHeaderName() = %q, want %q", got, tt.wantHeaderName)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SandboxHeaderName != "e2b-sandbox-id" {
		t.Errorf("DefaultConfig().SandboxHeaderName = %q, want %q", cfg.SandboxHeaderName, "e2b-sandbox-id")
	}
	if cfg.SandboxPortHeader != "e2b-sandbox-port" {
		t.Errorf("DefaultConfig().SandboxPortHeader = %q, want %q", cfg.SandboxPortHeader, "e2b-sandbox-port")
	}
	if cfg.HostHeaderName != "Host" {
		t.Errorf("DefaultConfig().HostHeaderName = %q, want %q", cfg.HostHeaderName, "Host")
	}
	if cfg.DefaultPort != "49983" {
		t.Errorf("DefaultConfig().DefaultPort = %q, want %q", cfg.DefaultPort, "49983")
	}
}
