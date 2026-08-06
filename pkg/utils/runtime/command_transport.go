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

package runtime

import (
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// runtimeCommandHTTPClient is the HTTP client used by RunCommandWithRuntime for the
// streaming Process/Start RPC.
//
// It deliberately does not use http.DefaultClient: the default client shares a
// process-wide connection pool with every other net/http user in the binary, so a
// long-lived streaming RPC whose result is carried in HTTP trailers becomes sensitive
// to unrelated traffic and to stale pooled connections. DisableKeepAlives keeps each
// command on its own connection, which makes a failure attributable to that single call.
//
// Intentionally no http.Client.Timeout is set: RunCommandWithRuntime wraps the caller's
// context with args.Timeout, which stays the single source of truth for deadlines
// (mirrors runtimeFilesHTTPClient).
//
// It is a variable so tests can substitute their own transport.
var runtimeCommandHTTPClient = &http.Client{
	Transport: newRuntimeCommandTransport(),
}

// newRuntimeCommandTransport builds the transport backing runtimeCommandHTTPClient.
// The dialer and handshake timeouts mirror http.DefaultTransport so the only intentional
// behavioral difference is connection reuse.
func newRuntimeCommandTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// gRPC carries the RPC status in HTTP trailers, which are the very last bytes of
		// the response. Never hand a command RPC a connection that another request has
		// already used (or that the peer may have half-closed while idle).
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// missingGRPCStatusTrailerMarker matches the message connect-go produces when a gRPC
// response body ends cleanly but carries no grpc-status trailer
// (connect's errTrailersWithoutGRPCStatus, see protocol_grpc.go). connect keeps that
// sentinel unexported and does not wrap it in anything comparable, so message matching
// is the only way to recognize it from the outside.
const missingGRPCStatusTrailerMarker = "no Grpc-Status trailer"

// IsMissingGRPCStatusTrailer reports whether err is the connect-go protocol error raised
// when the server closed the response stream without sending a grpc-status trailer.
//
// Such an error says nothing about whether the remote work happened: the request was
// accepted, the response was streamed and terminated gracefully, only the terminal status
// metadata is absent. Callers that already learned the outcome from the stream payload
// (for example a Process End event) can therefore treat it as non-fatal, while callers
// with no payload to fall back on must keep treating it as a failure.
//
// connect reports it with code Internal when no trailer at all was received, and Unknown
// when trailers arrived without grpc-status; both are accepted here.
func IsMissingGRPCStatusTrailer(err error) bool {
	if err == nil {
		return false
	}
	switch connect.CodeOf(err) {
	case connect.CodeInternal, connect.CodeUnknown:
	default:
		return false
	}
	return strings.Contains(err.Error(), missingGRPCStatusTrailerMarker)
}
