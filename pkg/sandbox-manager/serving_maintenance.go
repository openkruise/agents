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

package sandbox_manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandboxid"
	session "github.com/openkruise/agents/pkg/servingsession"
)

const servingIDPrefix = "serving-"

func (m *SandboxManager) initializeServingRuntime(ctx context.Context) error {
	backend := m.sessionBackend()
	if backend == nil {
		return nil
	}
	m.servingBackend = backend
	worker, err := m.allocateWorkerID(ctx, servingIDPrefix)
	if err != nil {
		return fmt.Errorf("allocate Serving ID worker: %w", err)
	}
	generate, err := sandboxid.NewGenerator(worker)
	if err != nil {
		return err
	}
	m.generateServingID = generate
	return nil
}

func (m *SandboxManager) runServingMaintenance(ctx context.Context, backend session.ServingSessionBackend) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := m.assignMissingServingIDs(ctx, backend); err != nil && ctx.Err() == nil {
			klog.FromContext(ctx).Error(err, "assign Serving identities")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *SandboxManager) assignMissingServingIDs(ctx context.Context, backend session.ServingSessionBackend) error {
	parents, err := backend.ListServings(ctx)
	if err != nil {
		return err
	}
	attempted := 0
	for _, parent := range parents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if parent.ID != "" || parent.Deleting {
			continue
		}
		if attempted == 32 {
			break
		}
		if m.generateServingID == nil {
			return fmt.Errorf("Serving ID generator is not initialized")
		}
		attempted++
		id, err := m.generateServingID()
		if err != nil {
			return err
		}
		if _, err := backend.AssignServingID(ctx, parent, servingIDPrefix+id); err != nil {
			if errors.Is(err, session.ErrServingConflict) || errors.Is(err, session.ErrServingNotFound) {
				continue
			}
			return err
		}
	}
	return nil
}
