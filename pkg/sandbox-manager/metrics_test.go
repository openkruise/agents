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
	"testing"
	"time"

	prometheusclient "github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func getGaugeValue(gaugeVec *prometheusclient.GaugeVec, labels ...string) float64 {
	gauge, err := gaugeVec.GetMetricWithLabelValues(labels...)
	if err != nil {
		return 0
	}
	var m dto.Metric
	if err := gauge.Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func getHistogramCount(histogramVec *prometheusclient.HistogramVec, labels ...string) uint64 {
	observer, err := histogramVec.GetMetricWithLabelValues(labels...)
	if err != nil {
		return 0
	}
	histogram, ok := observer.(prometheusclient.Metric)
	if !ok {
		return 0
	}
	var m dto.Metric
	if err := histogram.Write(&m); err != nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

func TestRecordClaimStageMetrics(t *testing.T) {
	namespace := "test-ns-claim"
	claimMetrics := infra.ClaimMetrics{
		Wait:          100 * time.Millisecond,
		PickAndLock:   200 * time.Millisecond,
		WaitReady:     300 * time.Millisecond,
		InitRuntime:   400 * time.Millisecond,
		CSIMount:      500 * time.Millisecond,
		SecurityToken: 50 * time.Millisecond,
		TrafficToken:  60 * time.Millisecond,
	}

	recordClaimStageMetrics(namespace, claimMetrics)

	stages := []string{"wait", "pick_and_lock", "wait_ready", "init_runtime", "csi_mount", "security_token", "traffic_token"}
	for _, stage := range stages {
		count := getHistogramCount(sandboxClaimStageDuration, namespace, stage)
		assert.GreaterOrEqual(t, count, uint64(1), "stage %s histogram sample count should be >= 1", stage)
	}
}

func TestRecordCloneStageMetrics(t *testing.T) {
	namespace := "test-ns-clone"
	cloneMetrics := infra.CloneMetrics{
		Wait:          150 * time.Millisecond,
		GetTemplate:   250 * time.Millisecond,
		CreateSandbox: 350 * time.Millisecond,
		WaitReady:     450 * time.Millisecond,
		InitRuntime:   550 * time.Millisecond,
		CSIMount:      650 * time.Millisecond,
		SecurityToken: 70 * time.Millisecond,
	}

	recordCloneStageMetrics(namespace, cloneMetrics)

	stages := []string{"wait", "get_template", "create_sandbox", "wait_ready", "init_runtime", "csi_mount", "security_token"}
	for _, stage := range stages {
		count := getHistogramCount(sandboxCloneStageDuration, namespace, stage)
		assert.GreaterOrEqual(t, count, uint64(1), "stage %s histogram sample count should be >= 1", stage)
	}
}

func TestPauseAndResumeMaxDurationMetrics(t *testing.T) {
	namespace := "test-ns-max-dur"

	sandboxPauseMaxDuration.WithLabelValues(namespace).Set(1.234)
	valPause := getGaugeValue(sandboxPauseMaxDuration, namespace)
	require.InDelta(t, 1.234, valPause, 0.001)

	sandboxResumeMaxDuration.WithLabelValues(namespace).Set(2.567)
	valResume := getGaugeValue(sandboxResumeMaxDuration, namespace)
	require.InDelta(t, 2.567, valResume, 0.001)
}

func TestRouteSyncDelayMetrics(t *testing.T) {
	namespace := "test-ns-route-sync"

	sandboxRouteSyncDelay.WithLabelValues(namespace).Set(0.456)
	val := getGaugeValue(sandboxRouteSyncDelay, namespace)
	require.InDelta(t, 0.456, val, 0.001)
}
