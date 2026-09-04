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

package storages

import (
	"os"
	"strings"

	"github.com/openkruise/agents/pkg/agent-runtime/common"
)

type initProviderFunc func(*StorageProvider)

var (
	initializeProviderFuncs = []initProviderFunc{}
	// driversConfig feeds PublishContext in the generated
	// NodePublishVolumeRequest (see volume_mount_provider.go); providers
	// may populate it as a hook for downstream pipeline enrichment. It is
	// written only during initialization and read-only afterwards.
	driversConfig = map[string]string{}
)

// buildProviderFuncs turns the DYNAMIC_STORAGE_DRIVER_LIST env value into
// provider registration functions. Names are trimmed, duplicates are
// dropped, and the customfuse driver is matched by its canonical name
// case-insensitively — a substring match would misclassify unrelated
// drivers whose names merely contain "customfuse", while a case-sensitive
// match would let a casing typo in the environment silently downgrade the
// customfuse driver to the unvalidated generic MountProvider.
//
// A missing or empty env value produces no registration functions, which
// leaves dynamic volume mounting unavailable: deployments must set the
// env explicitly (the sandbox-controller Deployment sets it).
func buildProviderFuncs(driverList string) []initProviderFunc {
	funcs := []initProviderFunc{}
	seen := map[string]bool{}
	for _, driverName := range strings.Split(driverList, ",") {
		drv := strings.TrimSpace(driverName)
		if drv == "" || seen[strings.ToLower(drv)] {
			continue
		}
		seen[strings.ToLower(drv)] = true
		provider := VolumeMountProvider(&MountProvider{})
		if strings.EqualFold(drv, "customfuseplugin.csi.openkruise.io") {
			// Register under the canonical lowercase name: PVs look the
			// driver up by Spec.CSI.Driver, and a registration key that
			// kept the user's casing would register successfully but
			// never be found.
			drv = "customfuseplugin.csi.openkruise.io"
			provider = &CustomFuseMountProvider{}
		}
		funcs = append(funcs, func(sp *StorageProvider) {
			sp.RegisterProvider(drv, provider)
		})
	}
	return funcs
}

func init() {
	initializeProviderFuncs = buildProviderFuncs(os.Getenv(common.ENV_DYNAMIC_STORAGE_DRIVER_LIST))
}
