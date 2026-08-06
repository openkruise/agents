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

package config

import (
	"encoding/json"
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
)

type InitRuntimeOptions struct {
	EnvVars     map[string]string `json:"envVars,omitempty"`
	AccessToken string            `json:"accessToken,omitempty"`
	ReInit      bool              `json:"-"`
	SkipRefresh bool              `json:"skipRefresh,omitempty"`
}

// NewDefaultAccessToken generates a default access token using UUID.
func NewDefaultAccessToken() string {
	return uuid.NewString()
}

const DefaultCSIMountConcurrency = 3

type CSIMountOptions struct {
	MountOptionList    []MountConfig `json:"mountOptionList"`
	MountOptionListRaw string        `json:"mountOptionListRaw"`    // the raw json string for mount options
	Concurrency        int           `json:"concurrency,omitempty"` // max concurrent CSI mount operations, 0 or negative means unlimited, default is DefaultCSIMountConcurrency
}

// MountConfig is a single resolved CSI mount intent: the driver that must serve
// it plus the CSI NodePublishVolume request to execute.
//
// The request is carried as the typed message rather than a pre-encoded blob, so
// each transport owns its own encoding: the runtime storage API sends the
// canonical protobuf JSON of the message (see runtime.CreateMountRequest), while
// the legacy sandbox-storage CLI encodes it to base64 protobuf as its last step
// before building the command line.
type MountConfig struct {
	Driver string
	// PublishRequest describes the volume to publish, as produced by
	// csiutils.CSIMountHandler.GenerateNodePublishVolumeRequest. It carries the
	// volume Secrets and PublishContext in clear text, so it must never be
	// rendered — neither by fmt nor by encoding/json: see String and MarshalJSON.
	PublishRequest *csi.NodePublishVolumeRequest
}

// redactedPublishRequest is the placeholder that stands in for the request in
// every rendering of a MountConfig. It avoids the characters encoding/json
// escapes (<, >, &) so that the same literal is greppable in both renderings.
const redactedPublishRequest = "[redacted]"

// mountConfigView is the redacted rendering of a MountConfig: the driver and the
// target path are useful for diagnostics, the request itself never is.
type mountConfigView struct {
	Driver     string `json:"driver"`
	TargetPath string `json:"targetPath,omitempty"`
	// PublishRequest only ever holds redactedPublishRequest.
	PublishRequest string `json:"publishRequest,omitempty"`
}

// MountConfig is an in-process type: it is never persisted nor put on a wire.
// The runtime wire type is runtime.CreateMountRequest, which carries the request
// losslessly as protobuf JSON. What reaches encoding/json here is therefore not
// a wire payload but a log line — klog serializes any value that is neither a
// fmt.Stringer nor a logr.Marshaler with encoding/json (see
// klog/internal/serialize.formatAsJSON), and sandbox-manager logs whole option
// structs that contain this one (infra.ClaimSandboxOptions,
// infra.CloneSandboxOptions), where the nested Stringer is never consulted.
// Encoding the CSI request there would write the volume Secrets to the log in
// clear text, so the JSON form is redacted exactly like String.
//
// Only the marshaller is implemented: the redacted form is deliberately not
// decodable, so no caller can mistake it for a wire format and end up mounting a
// request that silently lost its secrets.
var _ json.Marshaler = MountConfig{}

// MarshalJSON renders the redacted view. See the contract note above for why the
// CSI request is not encoded here.
func (m MountConfig) MarshalJSON() ([]byte, error) {
	view := mountConfigView{Driver: m.Driver, TargetPath: m.PublishRequest.GetTargetPath()}
	if m.PublishRequest != nil {
		view.PublishRequest = redactedPublishRequest
	}
	return json.Marshal(view)
}

// String implements fmt.Stringer so that logging a MountConfig — or a struct
// containing one — cannot leak the credential-bearing publish request: the
// generated CSI message renders its Secrets in full. It mirrors MarshalJSON so
// that both rendering paths agree.
func (m MountConfig) String() string {
	return fmt.Sprintf("MountConfig{Driver:%s, PublishRequest:%s, targetPath:%s}",
		m.Driver, redactedPublishRequest, m.PublishRequest.GetTargetPath())
}
