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
			name: "valid sandbox policy",
			cfg: &Config{
				HeaderMatchPolicy: HeaderMatchPolicySandbox,
			},
			wantErr: false,
		},
		{
			name: "valid host policy",
			cfg: &Config{
				HeaderMatchPolicy: HeaderMatchPolicyHost,
			},
			wantErr: false,
		},
		{
			name: "invalid policy",
			cfg: &Config{
				HeaderMatchPolicy: "invalid",
			},
			wantErr: true,
		},
		{
			name: "empty policy (zero value)",
			cfg: &Config{
				HeaderMatchPolicy: "",
			},
			wantErr: true,
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

func TestGetHeaderMatchName(t *testing.T) {
	tests := []struct {
		name           string
		cfg            *Config
		wantHeaderName string
	}{
		{
			name: "sandbox policy with empty config",
			cfg: &Config{
				HeaderMatchPolicy: HeaderMatchPolicySandbox,
				HeaderMatchName:   "",
			},
			wantHeaderName: "e2b-sandbox-id",
		},
		{
			name: "sandbox policy with custom name",
			cfg: &Config{
				HeaderMatchPolicy: HeaderMatchPolicySandbox,
				HeaderMatchName:   "custom-header",
			},
			wantHeaderName: "custom-header",
		},
		{
			name: "host policy with empty config",
			cfg: &Config{
				HeaderMatchPolicy: HeaderMatchPolicyHost,
				HeaderMatchName:   "",
			},
			wantHeaderName: "",
		},
		{
			name: "host policy with custom name",
			cfg: &Config{
				HeaderMatchPolicy: HeaderMatchPolicyHost,
				HeaderMatchName:   "x-custom-host",
			},
			wantHeaderName: "x-custom-host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.GetHeaderMatchName()
			if got != tt.wantHeaderName {
				t.Errorf("GetHeaderMatchName() = %q, want %q", got, tt.wantHeaderName)
			}
		})
	}
}
