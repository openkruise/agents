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
	"testing"
)

// applyFuncs registers every provider function against a fresh registry and
// returns it, so tests can assert on the final registration state.
func applyFuncs(t *testing.T, funcs []initProviderFunc) *StorageProvider {
	t.Helper()
	sp := NewStorageProvider()
	registry := sp.(*StorageProvider)
	for _, fn := range funcs {
		fn(registry)
	}
	return registry
}

func TestBuildProviderFuncs(t *testing.T) {
	tests := []struct {
		name         string
		driverList   string
		wantCount    int
		wantCustom   string
		wantMounted  []string
		absentDriver string
	}{
		{
			name:       "empty list produces no providers",
			driverList: "",
			wantCount:  0,
		},
		{
			name:        "plain drivers map to MountProvider",
			driverList:  "driver1,driver2,driver3",
			wantCount:   3,
			wantMounted: []string{"driver1", "driver2", "driver3"},
		},
		{
			name:        "canonical customfuse driver maps to CustomFuseMountProvider",
			driverList:  "nasplugin.csi.alibabacloud.com,customfuseplugin.csi.openkruise.io,ossplugin.csi.alibabacloud.com",
			wantCount:   3,
			wantCustom:  "customfuseplugin.csi.openkruise.io",
			wantMounted: []string{"nasplugin.csi.alibabacloud.com", "ossplugin.csi.alibabacloud.com"},
		},
		{
			name:         "substring containing customfuse is not the customfuse driver",
			driverList:   "my-customfuse-driver",
			wantCount:    1,
			wantMounted:  []string{"my-customfuse-driver"},
			absentDriver: "customfuseplugin.csi.openkruise.io",
		},
		{
			name:        "surrounding whitespace is trimmed",
			driverList:  " nasplugin.csi.alibabacloud.com , customfuseplugin.csi.openkruise.io ",
			wantCount:   2,
			wantCustom:  "customfuseplugin.csi.openkruise.io",
			wantMounted: []string{"nasplugin.csi.alibabacloud.com"},
		},
		{
			name:        "duplicate drivers are deduplicated",
			driverList:  "driver1,driver1,driver2",
			wantCount:   2,
			wantMounted: []string{"driver1", "driver2"},
		},
		{
			// A casing typo in the environment must not silently downgrade
			// the customfuse driver to the unvalidated generic provider,
			// and the registration key must be the canonical lowercase
			// name so PV lookups by Spec.CSI.Driver still find it.
			name:         "casing variant of canonical customfuse driver registers under canonical name",
			driverList:   "CUSTOMFUSEPLUGIN.CSI.OPENKRUISE.IO",
			wantCount:    1,
			wantCustom:   "customfuseplugin.csi.openkruise.io",
			absentDriver: "CUSTOMFUSEPLUGIN.CSI.OPENKRUISE.IO",
		},
		{
			name:       "casing duplicate of customfuse driver is deduplicated",
			driverList: "customfuseplugin.csi.openkruise.io,CUSTOMFUSEPLUGIN.CSI.OPENKRUISE.IO",
			wantCount:  1,
			wantCustom: "customfuseplugin.csi.openkruise.io",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			funcs := buildProviderFuncs(tt.driverList)
			if len(funcs) != tt.wantCount {
				t.Fatalf("expected %d provider funcs, got %d", tt.wantCount, len(funcs))
			}
			registry := applyFuncs(t, funcs)
			for _, drv := range tt.wantMounted {
				provider, exists := registry.GetProvider(drv)
				if !exists {
					t.Fatalf("driver %q not registered", drv)
				}
				if _, ok := provider.(*MountProvider); !ok {
					t.Errorf("driver %q expected *MountProvider, got %T", drv, provider)
				}
			}
			if tt.wantCustom != "" {
				provider, exists := registry.GetProvider(tt.wantCustom)
				if !exists {
					t.Fatalf("customfuse driver %q not registered", tt.wantCustom)
				}
				if _, ok := provider.(*CustomFuseMountProvider); !ok {
					t.Errorf("driver %q expected *CustomFuseMountProvider, got %T", tt.wantCustom, provider)
				}
			}
			if tt.absentDriver != "" {
				if _, exists := registry.GetProvider(tt.absentDriver); exists {
					t.Errorf("driver %q must not be registered", tt.absentDriver)
				}
			}
		})
	}
}
