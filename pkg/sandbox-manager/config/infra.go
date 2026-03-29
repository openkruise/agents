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
	// Resources specifies in-place resource update options.
	// +optional
	Resources *InplaceUpdateResourcesOptions `json:"resources,omitempty"`
}

type InplaceUpdateResourcesOptions struct {
	// ScaleFactor scales each container CPU by this factor, must be > 1.
	ScaleFactor float64
	// ReturnOnFeasible allows claim flow to return once resize is considered feasible
	// (e.g. PodResizeInProgress), instead of waiting for resize completion.
	ReturnOnFeasible bool
}
