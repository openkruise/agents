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

package opensandbox

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/servers/web"
)

const executionPort = 44772
const accessModeDirect = "direct"
const podIPNotAvailable = "sandbox Pod IP is not available"

// EndpointOptions selects one address strategy for this API deployment.
// GatewayURL is the external HTTP(S) origin; TLS terminates at that ingress.
type EndpointOptions struct {
	AccessMode string
	GatewayURL string
}

func (o EndpointOptions) validate() error {
	if o.AccessMode != "" && o.AccessMode != "ingress" && o.AccessMode != accessModeDirect {
		return fmt.Errorf("opensandbox access mode must be ingress or direct")
	}
	if o.GatewayURL != "" {
		u, err := url.Parse(o.GatewayURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
			return fmt.Errorf("opensandbox gateway URL must be an HTTP(S) origin without path, query or credentials")
		}
	}
	return nil
}

type Endpoint struct {
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers"`
}

func (s *Server) GetEndpoint(r *http.Request) (web.ApiResponse[Endpoint], *web.ApiError) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 500, Message: "user not found in request context"}
	}
	if r.URL.Query().Has("expires") {
		return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 501, Message: "signed endpoint URLs are not supported"}
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 400, Message: "port must be between 1 and 65535"}
	}
	if raw := r.URL.Query().Get("use_server_proxy"); raw != "" {
		proxy, err := strconv.ParseBool(raw)
		if err != nil {
			return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 400, Message: "invalid use_server_proxy"}
		}
		if proxy {
			return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 501, Message: "lifecycle server proxy is not supported"}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	connection, err := s.manager.GetSandboxConnection(ctx, NamespaceOfUser(user), user.ID.String(), r.PathValue(pathValueSandboxID))
	if err != nil {
		status := 500
		switch managererrors.GetErrCode(err) {
		case managererrors.ErrorNotFound, managererrors.ErrorNotAllowed:
			status = 404
		case managererrors.ErrorBadRequest:
			status = 400
		}
		// Missing and foreign-owned IDs deliberately have the same response.
		message := "sandbox lookup failed"
		if status == 404 {
			message = "sandbox not found"
		}
		return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: status, Message: message}
	}
	if connection.State == v1alpha1.SandboxStatePaused || connection.State == v1alpha1.SandboxStateDead {
		return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 409, Message: "sandbox is not running"}
	}
	if connection.IP == "" {
		return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 404, Message: podIPNotAvailable}
	}
	headers := map[string]string{}
	actualPort := port
	if port == executionPort || port == 18080 {
		actualPort = executionPort
		if connection.AccessToken == "" {
			return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 503, Message: "runtime execution credential is unavailable"}
		}
		headers["X-EXECD-ACCESS-TOKEN"] = connection.AccessToken
	}
	endpoint := net.JoinHostPort(connection.IP, strconv.Itoa(actualPort))
	if s.endpoints.AccessMode != accessModeDirect {
		if s.endpoints.GatewayURL == "" {
			return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 503, Message: "opensandbox gateway URL is not configured"}
		}
		u, err := url.Parse(s.endpoints.GatewayURL)
		if err != nil {
			return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 500, Message: "invalid gateway configuration"}
		}
		endpoint = u.Host
		headers["e2b-sandbox-id"] = connection.ID
		headers["e2b-sandbox-port"] = strconv.Itoa(actualPort)
		headers["X-Access-Token"] = connection.AccessToken
		if connection.RequireTrafficAuth {
			token, err := s.manager.RefreshTrafficAccessToken(ctx, sandboxmanager.RefreshTrafficAccessTokenOptions{Namespace: NamespaceOfUser(user), User: user.ID.String(), SandboxID: connection.ID})
			if err != nil {
				return web.ApiResponse[Endpoint]{}, &web.ApiError{Code: 503, Message: "gateway execution credential is unavailable"}
			}
			headers["e2b-traffic-access-token"] = token.Token
		}
	}
	return web.ApiResponse[Endpoint]{Code: http.StatusOK, Body: Endpoint{Endpoint: endpoint, Headers: headers}}, nil
}
