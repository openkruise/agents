package types

import (
	utils "github.com/openkruise/agents/pkg/utils/map"
)

const (
	AccessTokenAuthScopes = "AccessTokenAuth.Scopes"
)

type EnvVars map[string]string

type Defaults struct {
	EnvVars *utils.Map[string, string]
	User    string
	Workdir *string
}
