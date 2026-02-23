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

func init() {
	dynamicDriverList := strings.Split(os.Getenv(common.ENV_DYNAMIC_STORAGE_DRIVER_LIST), ",")
	for _, driverName := range dynamicDriverList {
		initializeProviderFuncs = append(initializeProviderFuncs,
			func(sp *StorageProvider) {
				sp.RegisterProvider(driverName, &MountProvider{})
			})
	}
}
