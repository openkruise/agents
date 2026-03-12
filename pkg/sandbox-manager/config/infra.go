package config

type InitRuntimeOptions struct {
	EnvVars     map[string]string `json:"envVars,omitempty"`
	AccessToken string            `json:"accessToken,omitempty"`
	ReInit      bool              `json:"-"`
}

type CSIMountOptions struct {
	MountOptionList []MountConfig `json:"mountOptionList"`
}

type MountConfig struct {
	Driver     string `json:"driver"`
	RequestRaw string `json:"requestRaw"`
}

type InplaceUpdateOptions struct {
	Image string
}
