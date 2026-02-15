package config

type InitRuntimeOptions struct {
	EnvVars     map[string]string
	AccessToken string
}

type CSIMountOptions struct {
	Driver     string
	RequestRaw string
}

type InplaceUpdateOptions struct {
	Image string
}
