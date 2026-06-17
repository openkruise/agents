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

package volume

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

const (
	resultSuccess = "success"
	resultFailure = "failure"
)

var (
	volumeOperationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "volume_operation_total",
			Help:        "Total number of volume operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "operation", "result"},
	)

	volumeOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "volume_operation_duration_seconds",
			Help:        "Duration of volume operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.001, 2, 12),
		},
		[]string{"namespace", "operation"},
	)
)

func init() {
	metrics.Registry.MustRegister(volumeOperationTotal, volumeOperationDuration)
}

// Manager wraps the Infrastructure interface and records Prometheus metrics on every call.
type Manager struct {
	infra infra.Infrastructure
}

// NewManager creates a new Manager backed by the given Infrastructure.
func NewManager(i infra.Infrastructure) *Manager {
	return &Manager{infra: i}
}

// CreateVolume creates a new persistent volume (PVC) for the user.
func (m *Manager) CreateVolume(ctx context.Context, opts infra.CreateVolumeOptions) (infra.VolumeInfo, error) {
	log := klog.FromContext(ctx)
	start := time.Now()

	info, err := m.infra.CreateVolume(ctx, opts)

	result := resultSuccess
	if err != nil {
		result = resultFailure
		log.Error(err, "failed to create volume", "namespace", opts.Namespace, "name", opts.Name)
	} else {
		log.V(5).Info("volume created", "namespace", opts.Namespace, "name", opts.Name, "volumeID", info.VolumeID)
	}

	volumeOperationDuration.WithLabelValues(opts.Namespace, "create").Observe(time.Since(start).Seconds())
	volumeOperationTotal.WithLabelValues(opts.Namespace, "create", result).Inc()

	return info, err
}

// ListVolumes returns all volumes registered under the given namespace.
func (m *Manager) ListVolumes(ctx context.Context, opts infra.ListVolumesOptions) ([]infra.VolumeInfo, error) {
	log := klog.FromContext(ctx)
	start := time.Now()

	volumes, err := m.infra.ListVolumes(ctx, opts)

	result := resultSuccess
	if err != nil {
		result = resultFailure
		log.Error(err, "failed to list volumes", "namespace", opts.Namespace)
	} else {
		log.V(5).Info("volumes listed", "namespace", opts.Namespace, "count", len(volumes))
	}

	volumeOperationDuration.WithLabelValues(opts.Namespace, "list").Observe(time.Since(start).Seconds())
	volumeOperationTotal.WithLabelValues(opts.Namespace, "list", result).Inc()

	return volumes, err
}

// GetVolume returns metadata for a single volume by its ID within the given namespace.
func (m *Manager) GetVolume(ctx context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error) {
	log := klog.FromContext(ctx)
	start := time.Now()

	info, err := m.infra.GetVolume(ctx, opts)

	result := resultSuccess
	if err != nil {
		result = resultFailure
		log.Error(err, "failed to get volume", "namespace", opts.Namespace, "volumeID", opts.VolumeID)
	} else {
		log.V(5).Info("volume retrieved", "namespace", opts.Namespace, "volumeID", opts.VolumeID)
	}

	volumeOperationDuration.WithLabelValues(opts.Namespace, "get").Observe(time.Since(start).Seconds())
	volumeOperationTotal.WithLabelValues(opts.Namespace, "get", result).Inc()

	return info, err
}

// DeleteVolume unregisters a volume. If force is false and the volume is mounted,
// it returns a conflict error. If force is true, the volume is removed regardless.
func (m *Manager) DeleteVolume(ctx context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error) {
	log := klog.FromContext(ctx)
	start := time.Now()

	deleteResult, err := m.infra.DeleteVolume(ctx, opts)

	result := resultSuccess
	if err != nil {
		result = resultFailure
		log.Error(err, "failed to delete volume", "namespace", opts.Namespace, "volumeID", opts.VolumeID, "force", opts.Force)
	} else {
		log.V(5).Info("volume deleted", "namespace", opts.Namespace, "volumeID", opts.VolumeID, "forced", deleteResult.ForcedDelete)
	}

	volumeOperationDuration.WithLabelValues(opts.Namespace, "unregister").Observe(time.Since(start).Seconds())
	volumeOperationTotal.WithLabelValues(opts.Namespace, "unregister", result).Inc()

	return deleteResult, err
}
