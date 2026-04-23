package adapters

import (
	"fmt"
	"strconv"
	"strings"
)

// E2BMapper is part of proxy.RequestAdapter
type E2BMapper interface {
	Map(req *ParsedRequest) (
		sandboxID string, sandboxPort int, extraHeaders map[string]string, err error)
	IsSandboxRequest(authority, path string, port int) bool
}

var DefaultAdapterFactory = NewE2BAdapter

type E2BAdapter struct {
	Port       int
	native     *NativeE2BAdapter
	customized *CustomizedE2BAdapter
}

func NewE2BAdapter(port int) *E2BAdapter {
	return &E2BAdapter{
		Port:       port,
		native:     &NativeE2BAdapter{},
		customized: &CustomizedE2BAdapter{},
	}
}

// E2BAdapterOptions holds configurable options for creating an E2BAdapter
// with custom header names and default port settings.
type E2BAdapterOptions struct {
	SandboxIDHeader   string
	SandboxPortHeader string
	HostHeader        string
	DefaultPort       int
}

// NewE2BAdapterWithOptions creates an E2BAdapter with configurable NativeE2BAdapter options.
// This allows the sandbox-gateway to configure custom header names and default port.
func NewE2BAdapterWithOptions(port int, opts E2BAdapterOptions) *E2BAdapter {
	return &E2BAdapter{
		Port: port,
		native: &NativeE2BAdapter{
			SandboxIDHeader:   opts.SandboxIDHeader,
			SandboxPortHeader: opts.SandboxPortHeader,
			HostHeader:        opts.HostHeader,
			DefaultPort:       opts.DefaultPort,
		},
		customized: &CustomizedE2BAdapter{},
	}
}

func (a *E2BAdapter) Map(req *ParsedRequest) (
	sandboxID string, sandboxPort int, extraHeaders map[string]string, err error) {
	return a.ChooseAdapter(req.Path).Map(req)
}

func (a *E2BAdapter) IsSandboxRequest(authority, path string, port int) bool {
	return a.ChooseAdapter(path).IsSandboxRequest(authority, path, port)
}

func (a *E2BAdapter) Entry() string {
	return fmt.Sprintf("127.0.0.1:%d", a.Port)
}

func (a *E2BAdapter) ChooseAdapter(path string) E2BMapper {
	if strings.HasPrefix(path, CustomPrefix) {
		return a.customized
	}
	return a.native
}

// ParsedRequest holds normalized HTTP request info extracted from raw headers.
// Any data plane (ext_proc, go filter, nginx plugin, etc.) should convert its native
// header format into a flat map[string]string and call ParseRequest to get this struct.
type ParsedRequest struct {
	Scheme    string
	Authority string
	Path      string
	Port      int
	Headers   map[string]string
}

// ParseRequest normalizes raw HTTP headers into a ParsedRequest.
// The input headers map should use standard HTTP/2 pseudo-header keys
// (:scheme, :authority, :path) plus regular headers (e.g., "host").
// Logic:
//   - Extract scheme, authority, path from pseudo-headers
//   - Fallback authority to "host" header if :authority is absent
//   - Parse port from authority (host:port), or infer default from scheme
func (a *E2BAdapter) ParseRequest(headers map[string]string) *ParsedRequest {
	parsed := &ParsedRequest{
		Headers: headers,
	}

	parsed.Scheme = headers[":scheme"]
	parsed.Authority = headers[":authority"]
	parsed.Path = headers[":path"]

	// Fallback: if :authority is absent, use the "host" header
	if parsed.Authority == "" {
		parsed.Authority = headers["host"]
	}

	// Extract port from authority
	if parsed.Authority != "" {
		// Check if it contains a port number (host:port format)
		parts := strings.Split(parsed.Authority, ":")
		if len(parts) > 1 {
			// Try to parse the port number
			if p, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
				parsed.Port = p
			}
			return parsed
		}

		// If no port is explicitly specified, determine the default port based on the scheme
		switch parsed.Scheme {
		case "https", "wss":
			parsed.Port = 443
		case "http", "ws":
			parsed.Port = 80
		}
	}
	return parsed
}
