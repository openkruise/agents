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

// Package customfusesidecar implements the CSI node server that runs inside
// the csi-sidecar container of a sandbox pod. It receives NodePublishVolume
// requests from the storage CLI over the per-driver unix socket and forwards
// them to mount-proxy-server, which executes the FUSE entrypoint.
//
// The wire protocol (JSON over a unix socket, newline-terminated) mirrors the
// alibaba-cloud-csi-driver mount-proxy protocol so that the sidecar can talk
// to an unmodified mount-proxy-server binary.
package customfusesidecar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	// proxyMessageEnd terminates each JSON frame on the socket.
	proxyMessageEnd = '\n'
	// proxyTimeout is the upper bound for one proxy RPC. The mount-proxy
	// server itself runs the entrypoint with a 30s handle timeout, so the
	// client deadline must be longer than that.
	proxyTimeout = 35 * time.Second
)

// maxProxyMsgSize bounds a single proxy request/response (256MB, same
// upper bound as the mount-proxy server). A var, not a const, so tests can
// shrink it instead of allocating a 256MB payload. Production code MUST NOT
// modify it.
var maxProxyMsgSize = 1 << 28

type proxyMethod string

const (
	proxyMount proxyMethod = "mount"
	proxyPing  proxyMethod = "ping"
)

type proxyRequest struct {
	Header proxyHeader `json:"header"`
	Body   any         `json:"body,omitempty"`
}

type proxyHeader struct {
	Method proxyMethod `json:"method,omitempty"`
}

type proxyResponse struct {
	Seq   int64  `json:"seq,omitempty"`
	Error string `json:"error,omitempty"`
}

type proxyMountRequest struct {
	Source      string            `json:"source,omitempty"`
	Target      string            `json:"target,omitempty"`
	Fstype      string            `json:"fstype,omitempty"`
	Options     []string          `json:"options,omitempty"`
	MountFlags  []string          `json:"mountFlags,omitempty"`
	Secrets     map[string]string `json:"secrets,omitempty"`
	MetricsPath string            `json:"metricsPath,omitempty"`
	VolumeID    string            `json:"volumeID,omitempty"`
}

// proxyDoRequestFn is the indirection used by proxyClient.Mount to issue a
// request. It is a package variable so tests can substitute a fake without
// binding to a real unix socket. Production code MUST NOT reassign it.
var proxyDoRequestFn = func(c *proxyClient, ctx context.Context, req *proxyRequest) (*proxyResponse, error) {
	return c.doRequest(ctx, req)
}

// proxyClient talks to a mount-proxy-server over its unix socket.
type proxyClient struct {
	raddr   net.UnixAddr
	timeout time.Duration
}

func newProxyClient(socketPath string) *proxyClient {
	return &proxyClient{
		raddr:   net.UnixAddr{Name: socketPath, Net: "unix"},
		timeout: proxyTimeout,
	}
}

// Mount sends a mount request and returns an error carrying the server-side
// failure message, if any.
func (c *proxyClient) Mount(ctx context.Context, req *proxyMountRequest) error {
	resp, err := proxyDoRequestFn(c, ctx, &proxyRequest{
		Header: proxyHeader{Method: proxyMount},
		Body:   req,
	})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}

func (c *proxyClient) doRequest(ctx context.Context, req *proxyRequest) (*proxyResponse, error) {
	// One deadline bounds the whole RPC — connection establishment and the
	// request/response exchange — so the total wait never exceeds
	// c.timeout. The dial uses the same context: a unix connect can block
	// when the server's listen backlog is full, and a separate Dialer
	// timeout would add up with the deadline below, doubling the worst
	// case.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.raddr.Name)
	if err != nil {
		return nil, fmt.Errorf("dial mount proxy %s: %w", c.raddr.Name, err)
	}
	defer conn.Close()

	deadline, ok := ctx.Deadline()
	if ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set deadline: %w", err)
		}
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode proxy request: %w", err)
	}
	if _, err := conn.Write(append(data, proxyMessageEnd)); err != nil {
		return nil, fmt.Errorf("send proxy request: %w", err)
	}

	var resp proxyResponse
	if err := readProxyMsg(conn, &resp); err != nil {
		return nil, fmt.Errorf("read proxy response: %w", err)
	}
	return &resp, nil
}

// readProxyMsg decodes one JSON message and verifies that a message-end byte
// follows, mirroring the mount-proxy server's framing.
func readProxyMsg(r io.Reader, msg any) error {
	lr := io.LimitedReader{R: r, N: int64(maxProxyMsgSize)}
	dec := json.NewDecoder(&lr)
	if err := dec.Decode(msg); err != nil {
		if lr.N <= 0 {
			return errors.New("proxy message too large")
		}
		return fmt.Errorf("decode proxy message: %w", err)
	}

	var p [32]byte
	n, err := io.MultiReader(dec.Buffered(), &lr).Read(p[:])
	if err != nil {
		return fmt.Errorf("read proxy message end: %w", err)
	}
	// The frame must end with exactly one message-end byte; anything more is
	// a protocol violation that would desynchronize the request/response
	// pairing on this connection.
	if n != 1 || p[0] != proxyMessageEnd {
		return errors.New("missing message end after proxy message")
	}
	return nil
}
