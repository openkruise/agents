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

package util

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	agents "github.com/openkruise/agents/api/v1alpha1"
)

// SessionServiceMaxSurge resolves the rollout budget against the desired replicas.
func SessionServiceMaxSurge(parent *agents.SessionService) (int32, error) {
	value := intstr.FromString("25%")
	if parent.Spec.UpdateStrategy.MaxSurge != nil {
		value = *parent.Spec.UpdateStrategy.MaxSurge
	}
	if value.Type == intstr.Int {
		if value.IntVal <= 0 {
			return 0, fmt.Errorf("maxSurge must be positive")
		}
		return value.IntVal, nil
	}
	if value.Type != intstr.String {
		return 0, fmt.Errorf("invalid maxSurge type")
	}
	text := value.StrVal
	if !strings.HasSuffix(text, "%") {
		return 0, fmt.Errorf("maxSurge must be an integer or percentage")
	}
	digits := strings.TrimSuffix(text, "%")
	for _, ch := range digits {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid maxSurge percentage")
		}
	}
	percent, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || percent <= 0 {
		return 0, fmt.Errorf("maxSurge percentage must be positive")
	}
	desired := int64(ptr.Deref(parent.Spec.Replicas, 1))
	// Divide before multiplication to avoid overflowing large valid percentages.
	if desired > 0 && percent > (int64(math.MaxInt32)*100)/desired {
		return 0, fmt.Errorf("maxSurge exceeds supported replica range")
	}
	result := (desired*percent + 99) / 100
	if result < 0 || result > math.MaxInt32 {
		return 0, fmt.Errorf("maxSurge exceeds supported replica range")
	}
	return int32(result), nil
}
