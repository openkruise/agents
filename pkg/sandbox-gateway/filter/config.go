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

package filter

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	v3 "github.com/cncf/xds/go/xds/type/v3"
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"golang.org/x/net/http/httpguts"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/openkruise/agents/pkg/identity/oidc"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
)

const (
	DefaultHostHeaderName           = "Host"
	DefaultSandboxHeaderName        = "e2b-sandbox-id"
	DefaultSandboxPortHeader        = "e2b-sandbox-port"
	DefaultSandboxPort              = "49983"
	DefaultTrafficAccessTokenHeader = "e2b-traffic-access-token"
)

// JWTAuthManager configures and exposes the process-wide JWT verifier.
type JWTAuthManager interface {
	Configure(enabled bool) error
	Current() oidc.Verifier
}

// Config holds the filter configuration
type Config struct {
	// SandboxHeaderName is the header name for sandbox ID (checked first)
	SandboxHeaderName string `json:"sandbox-header-name,omitempty"`
	// SandboxPortHeader is the header name for sandbox port
	SandboxPortHeader string `json:"sandbox-port-header,omitempty"`
	// HostHeaderName is the header name for host matching (fallback when sandbox header not found)
	HostHeaderName string `json:"host-header-name,omitempty"`
	// DefaultPort is the default port if not specified
	DefaultPort string `json:"default-port,omitempty"`
	// EnableAuth enables the UUID access-token baseline for routes that have not
	// opted into JWT enforcement. When disabled (default), those routes skip token
	// validation for backward compatibility.
	EnableAuth bool `json:"enable-auth,omitempty"`
	// EnableJWTAuth initializes the process-wide JWT capability, which enforces
	// JWT verification on the routes that opt in through the Sandbox annotation.
	// It is independent of EnableAuth: the two switches govern disjoint sets of
	// routes, so enabling JWT never changes how a non-opted-in route is
	// authenticated. That keeps both migration paths compatible: a deployment
	// already on UUID keeps its baseline, and a deployment with authentication
	// off keeps letting existing traffic through.
	EnableJWTAuth bool `json:"enable-jwt-auth,omitempty"`
	// TrafficAccessTokenHeader is the request header carrying the traffic access JWT.
	TrafficAccessTokenHeader string `json:"traffic-access-token-header,omitempty"`
	// EnableRuntimeMTLS routes requests to the agent-runtime port through the mTLS upstream cluster.
	EnableRuntimeMTLS bool `json:"enable-runtime-mtls,omitempty"`
	// EnableWakeOnTraffic enables wake-on-traffic for paused sandboxes.
	// When true, the gateway will attempt to resume a paused sandbox by
	// patching Spec.Paused=false when traffic arrives.
	// Listener-level only: per-route configuration is NOT supported. Merge
	// only lets an explicit true override the listener value, so a
	// per-route false cannot disable wake enabled at the listener level.
	EnableWakeOnTraffic bool `json:"enable-wake-on-traffic,omitempty"`
	// WakeTimeoutSeconds is the max time (in seconds) to wait for a sandbox
	// to resume before returning an error. Defaults to 60.
	// Listener-level only: per-route configuration is NOT supported. Every
	// parsed config carries the default 60, so any per-route config block
	// would unintentionally reset a listener-level timeout.
	WakeTimeoutSeconds int `json:"wake-timeout-seconds,omitempty"`
}

// DefaultConfig returns default configuration
func DefaultConfig() *Config {
	return &Config{
		SandboxHeaderName:        DefaultSandboxHeaderName,
		SandboxPortHeader:        DefaultSandboxPortHeader,
		HostHeaderName:           DefaultHostHeaderName,
		DefaultPort:              DefaultSandboxPort,
		TrafficAccessTokenHeader: DefaultTrafficAccessTokenHeader,
		WakeTimeoutSeconds:       60,
	}
}

// Validate checks configuration validity
func (c *Config) Validate() error {
	headerName := c.GetTrafficAccessTokenHeader()
	if !httpguts.ValidHeaderFieldName(headerName) || strings.HasPrefix(headerName, ":") {
		return fmt.Errorf("traffic-access-token-header %q is not a valid HTTP header name", headerName)
	}
	if strings.EqualFold(headerName, accessTokenHeader) {
		return fmt.Errorf("traffic-access-token-header must differ from %s", accessTokenHeader)
	}
	return nil
}

// GetSandboxHeaderName returns the effective sandbox header name
func (c *Config) GetSandboxHeaderName() string {
	if c.SandboxHeaderName != "" {
		return c.SandboxHeaderName
	}
	return DefaultSandboxHeaderName
}

// GetHostHeaderName returns the effective host header name
func (c *Config) GetHostHeaderName() string {
	if c.HostHeaderName != "" {
		return c.HostHeaderName
	}
	return DefaultHostHeaderName
}

// GetSandboxPortHeader returns the effective sandbox port header name
func (c *Config) GetSandboxPortHeader() string {
	if c.SandboxPortHeader != "" {
		return c.SandboxPortHeader
	}
	return DefaultSandboxPortHeader
}

// GetDefaultPort returns the default port as an integer
func (c *Config) GetDefaultPort() int {
	if c.DefaultPort != "" {
		if p, err := strconv.Atoi(c.DefaultPort); err == nil {
			return p
		}
	}
	p, _ := strconv.Atoi(DefaultSandboxPort)
	return p
}

// GetTrafficAccessTokenHeader returns the configured JWT header name.
func (c *Config) GetTrafficAccessTokenHeader() string {
	if c.TrafficAccessTokenHeader != "" {
		return strings.ToLower(c.TrafficAccessTokenHeader)
	}
	return DefaultTrafficAccessTokenHeader
}

// GetWakeTimeoutSeconds returns the wake timeout in seconds, defaulting to 60.
func (c *Config) GetWakeTimeoutSeconds() int {
	if c.WakeTimeoutSeconds > 0 {
		return c.WakeTimeoutSeconds
	}
	return 60
}

// FilterConfig wraps Config and holds the adapter created from the config
type FilterConfig struct {
	*Config
	Adapter                          *adapters.E2BAdapter
	jwtAuthManager                   JWTAuthManager
	trafficAccessTokenHeaderExplicit bool
	// enableAuthExplicit records that enable-auth was present in the parsed
	// config, so Merge can tell an explicit false from an omitted field.
	enableAuthExplicit bool
}

// NewFilterConfig creates a FilterConfig with an adapter built from the config values
func NewFilterConfig(cfg *Config) *FilterConfig {
	return newFilterConfig(cfg, nil)
}

func newFilterConfig(cfg *Config, jwtAuthManager JWTAuthManager) *FilterConfig {
	adapter := adapters.NewE2BAdapterWithOptions(
		0, // port not used by gateway
		adapters.E2BAdapterOptions{
			SandboxIDHeader:   cfg.GetSandboxHeaderName(),
			SandboxPortHeader: cfg.GetSandboxPortHeader(),
			HostHeader:        cfg.GetHostHeaderName(),
			DefaultPort:       cfg.GetDefaultPort(),
		},
	)
	return &FilterConfig{
		Config:                           cfg,
		Adapter:                          adapter,
		jwtAuthManager:                   jwtAuthManager,
		trafficAccessTokenHeaderExplicit: cfg.TrafficAccessTokenHeader != "",
		enableAuthExplicit:               cfg.EnableAuth,
	}
}

type ConfigParser struct {
	jwtAuthManager JWTAuthManager
}

// NewConfigParser creates a parser wired to the process-wide JWT manager.
func NewConfigParser(jwtAuthManager JWTAuthManager) *ConfigParser {
	return &ConfigParser{jwtAuthManager: jwtAuthManager}
}

func (p *ConfigParser) Parse(any *anypb.Any, callbacks api.ConfigCallbackHandler) (interface{}, error) {
	cfg := DefaultConfig()

	// Unmarshal the xds.type.v3.TypedStruct protobuf message
	typedStruct := &v3.TypedStruct{}
	if err := any.UnmarshalTo(typedStruct); err != nil {
		return nil, fmt.Errorf("failed to unmarshal TypedStruct: %w", err)
	}

	// Get the value from TypedStruct which contains the actual config as Struct
	valueStruct := typedStruct.GetValue()
	if valueStruct == nil {
		// No value field, use defaults
		if callbacks != nil {
			if err := p.configureJWT(cfg); err != nil {
				return nil, err
			}
		}
		parsed := newFilterConfig(cfg, p.jwtAuthManager)
		parsed.trafficAccessTokenHeaderExplicit = false
		parsed.enableAuthExplicit = false
		return parsed, nil
	}

	// Convert the struct to JSON
	values := valueStruct.AsMap()
	_, jwtModeExplicit := values["enable-jwt-auth"]
	_, tokenHeaderExplicit := values["traffic-access-token-header"]
	_, runtimeMTLSExplicit := values["enable-runtime-mtls"]
	_, enableAuthExplicit := values["enable-auth"]
	if callbacks == nil && jwtModeExplicit {
		return nil, fmt.Errorf("enable-jwt-auth is process-wide and cannot be configured per route")
	}
	if callbacks == nil && runtimeMTLSExplicit {
		return nil, fmt.Errorf("enable-runtime-mtls is process-wide and cannot be configured per route")
	}
	configBytes, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config value to JSON: %w", err)
	}

	// Parse actual config from JSON
	if len(configBytes) > 0 && string(configBytes) != "null" {
		if err := json.Unmarshal(configBytes, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config: %w", err)
		}
	}

	// Validate
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if callbacks != nil {
		if err := p.configureJWT(cfg); err != nil {
			return nil, err
		}
	}
	parsed := newFilterConfig(cfg, p.jwtAuthManager)
	parsed.trafficAccessTokenHeaderExplicit = tokenHeaderExplicit
	parsed.enableAuthExplicit = enableAuthExplicit
	return parsed, nil
}

func (p *ConfigParser) configureJWT(cfg *Config) error {
	if p.jwtAuthManager == nil {
		if cfg.EnableJWTAuth {
			return fmt.Errorf("JWT authentication manager is not configured")
		}
		return nil
	}
	if err := p.jwtAuthManager.Configure(cfg.EnableJWTAuth); err != nil {
		return fmt.Errorf("configure JWT authentication: %w", err)
	}
	return nil
}

func (p *ConfigParser) Merge(parent interface{}, child interface{}) interface{} {
	parentCfg := parent.(*FilterConfig)
	childCfg := child.(*FilterConfig)

	// Child overrides parent for all fields
	merged := DefaultConfig()
	*merged = *parentCfg.Config

	if childCfg.SandboxHeaderName != "" {
		merged.SandboxHeaderName = childCfg.SandboxHeaderName
	}
	if childCfg.SandboxPortHeader != "" {
		merged.SandboxPortHeader = childCfg.SandboxPortHeader
	}
	if childCfg.HostHeaderName != "" {
		merged.HostHeaderName = childCfg.HostHeaderName
	}
	if childCfg.DefaultPort != "" {
		merged.DefaultPort = childCfg.DefaultPort
	}
	if childCfg.enableAuthExplicit {
		merged.EnableAuth = childCfg.EnableAuth
	}
	if childCfg.EnableJWTAuth {
		merged.EnableJWTAuth = childCfg.EnableJWTAuth
	}
	if childCfg.trafficAccessTokenHeaderExplicit {
		merged.TrafficAccessTokenHeader = childCfg.TrafficAccessTokenHeader
	}
	// Wake-on-traffic is listener-level configuration: per-route overrides
	// are not supported. Only an explicit true / positive value propagates;
	// a per-route config can neither disable wake nor reset the timeout.
	if childCfg.EnableWakeOnTraffic {
		merged.EnableWakeOnTraffic = childCfg.EnableWakeOnTraffic
	}
	if childCfg.WakeTimeoutSeconds > 0 {
		merged.WakeTimeoutSeconds = childCfg.WakeTimeoutSeconds
	}

	jwtAuthManager := parentCfg.jwtAuthManager
	if childCfg.jwtAuthManager != nil {
		jwtAuthManager = childCfg.jwtAuthManager
	}
	return newFilterConfig(merged, jwtAuthManager)
}
