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
	driversConfig           = map[string]string{}
)

// driverListFromEnv reads the configured CSI driver names.
// ENV_DYNAMIC_STORAGE_DRIVER_LIST is the deprecated spelling of
// ENV_ON_DEMAND_MOUNT_DRIVER_LIST, so it is only read when the current name is unset.
// Empty entries are dropped; an unset variable configures no drivers at all.
func driverListFromEnv() []string {
	list := os.Getenv(common.ENV_ON_DEMAND_MOUNT_DRIVER_LIST)
	if list == "" {
		list = os.Getenv(common.ENV_DYNAMIC_STORAGE_DRIVER_LIST)
	}

	var drivers []string
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			drivers = append(drivers, name)
		}
	}
	return drivers
}

func init() {
	for _, driverName := range driverListFromEnv() {
		initializeProviderFuncs = append(initializeProviderFuncs,
			func(sp *StorageProvider) {
				sp.RegisterProvider(driverName, &MountProvider{})
			})
	}
}
