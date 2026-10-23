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

package e2b

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestAPIHandlerMetrics(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()
	user := adminTestUser()
	templateName := "handler-metrics-template"

	t.Run("create before path selection records neither claim nor clone", func(t *testing.T) {
		claimBefore := familyCounter(t, "sandbox_claim_creation_responses")
		cloneBefore := familyCounter(t, "sandbox_clone_total")

		_, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{TemplateID: templateName}, nil, nil))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusUnauthorized, apiErr.Code)

		_, apiErr = controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{}, nil, user))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)

		_, apiErr = controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{TemplateID: "missing-template"}, nil, user))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)

		assert.Equal(t, claimBefore, familyCounter(t, "sandbox_claim_creation_responses"))
		assert.Equal(t, cloneBefore, familyCounter(t, "sandbox_clone_total"))
	})

	// The pool helper's cleanup deletes each generated sandbox by name and fails
	// once the cleanup-delete case has already removed one. The envtest process
	// stops with this test, so the objects do not need an exact-name delete.
	_ = CreateSandboxPool(t, controller, templateName, 3)

	t.Run("claim validation failure records neither counter nor duration", func(t *testing.T) {
		claimBefore := familyCounter(t, "sandbox_claim_creation_responses")
		histBefore := familyHistogramCount(t, "sandbox_claim_duration_seconds")

		_, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: templateName,
			Metadata: map[string]string{
				models.ExtensionKeySandboxName: "my-sbx",
			},
		}, nil, user))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Equal(t, claimBefore, familyCounter(t, "sandbox_claim_creation_responses"))
		assert.Equal(t, histBefore, familyHistogramCount(t, "sandbox_claim_duration_seconds"))
	})

	var sandboxID string
	t.Run("claim success counts and observes duration", func(t *testing.T) {
		before := labeledCounter(t, "sandbox_claim_creation_responses", map[string]string{
			"result": "success",
		})
		histBefore := familyHistogramCount(t, "sandbox_claim_duration_seconds")
		cloneBefore := familyCounter(t, "sandbox_clone_total")

		resp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: templateName,
			Metadata: map[string]string{
				models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
			},
		}, nil, user))
		require.Nil(t, apiErr)
		require.NotNil(t, resp.Body)
		sandboxID = resp.Body.SandboxID

		assert.Equal(t, before+1, labeledCounter(t, "sandbox_claim_creation_responses", map[string]string{
			"result": "success",
		}))
		assert.Equal(t, histBefore+1, familyHistogramCount(t, "sandbox_claim_duration_seconds"))
		assert.Equal(t, cloneBefore, familyCounter(t, "sandbox_clone_total"))
	})
	require.NotEmpty(t, sandboxID)
	EnableWaitSim(t, controller, sandboxID)

	t.Run("pause before the sandbox is known records nothing", func(t *testing.T) {
		counterBefore := familyCounter(t, "sandbox_pause_responses")
		histBefore := familyHistogramCount(t, "sandbox_pause_duration_seconds")
		_, apiErr := controller.PauseSandbox(NewRequest(t, nil, nil, map[string]string{
			"sandboxID": "missing-sandbox",
		}, user))
		require.NotNil(t, apiErr)
		assert.Equal(t, counterBefore, familyCounter(t, "sandbox_pause_responses"))
		assert.Equal(t, histBefore, familyHistogramCount(t, "sandbox_pause_duration_seconds"))
	})

	t.Run("pause success counts and observes duration", func(t *testing.T) {
		before := labeledCounter(t, "sandbox_pause_responses", map[string]string{
			"result": "success",
		})
		histBefore := familyHistogramCount(t, "sandbox_pause_duration_seconds")
		pauseSandboxHelper(t, controller, fc, sandboxID, false, false, user)
		assert.Equal(t, before+1, labeledCounter(t, "sandbox_pause_responses", map[string]string{
			"result": "success",
		}))
		assert.Equal(t, histBefore+1, familyHistogramCount(t, "sandbox_pause_duration_seconds"))
	})

	t.Run("connect resume is not a resume request and records nothing", func(t *testing.T) {
		counterBefore := familyCounter(t, "sandbox_resume_responses")
		histBefore := familyHistogramCount(t, "sandbox_resume_duration_seconds")
		go UpdateSandboxWhen(t, fc, sandboxID, waitForResumeUpdate(controller, true),
			DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, "", metav1.ConditionTrue))
		resp, apiErr := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
			TimeoutSeconds: models.DefaultTimeoutSeconds,
		}, map[string]string{"sandboxID": sandboxID}, user))
		require.Nil(t, apiErr)
		require.Equal(t, http.StatusCreated, resp.Code)
		assert.Equal(t, counterBefore, familyCounter(t, "sandbox_resume_responses"))
		assert.Equal(t, histBefore, familyHistogramCount(t, "sandbox_resume_duration_seconds"))
	})

	t.Run("resume success counts and observes duration", func(t *testing.T) {
		pauseSandboxHelper(t, controller, fc, sandboxID, false, false, user)
		before := labeledCounter(t, "sandbox_resume_responses", map[string]string{
			"result": "success",
		})
		histBefore := familyHistogramCount(t, "sandbox_resume_duration_seconds")
		go UpdateSandboxWhen(t, fc, sandboxID, waitForResumeUpdate(controller, true),
			DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, "", metav1.ConditionTrue))
		resp, apiErr := controller.ResumeSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
			TimeoutSeconds: models.DefaultTimeoutSeconds,
		}, map[string]string{"sandboxID": sandboxID}, user))
		require.Nil(t, apiErr)
		require.Equal(t, http.StatusNoContent, resp.Code)
		assert.Equal(t, before+1, labeledCounter(t, "sandbox_resume_responses", map[string]string{
			"result": "success",
		}))
		assert.Equal(t, histBefore+1, familyHistogramCount(t, "sandbox_resume_duration_seconds"))
	})

	t.Run("delete not found records nothing", func(t *testing.T) {
		counterBefore := familyCounter(t, "sandbox_delete_responses")
		histBefore := familyHistogramCount(t, "sandbox_delete_duration_seconds")
		resp, apiErr := controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
			"sandboxID": "missing-sandbox",
		}, user))
		require.Nil(t, apiErr)
		assert.Equal(t, http.StatusNoContent, resp.Code)
		assert.Equal(t, counterBefore, familyCounter(t, "sandbox_delete_responses"))
		assert.Equal(t, histBefore, familyHistogramCount(t, "sandbox_delete_duration_seconds"))
	})

	t.Run("cleanup delete is not a delete request and records nothing", func(t *testing.T) {
		resp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: templateName,
			Metadata: map[string]string{
				models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
			},
		}, nil, user))
		require.Nil(t, apiErr)
		ctx := context.WithValue(t.Context(), userContextKey, user)
		sbx, getErr := controller.getSandboxOfUser(ctx, resp.Body.SandboxID, claimedSandboxStates)
		require.Nil(t, getErr)
		counterBefore := familyCounter(t, "sandbox_delete_responses")
		histBefore := familyHistogramCount(t, "sandbox_delete_duration_seconds")
		require.True(t, controller.cleanUpSandboxAfterFailure(ctx, sbx, user, klog.Background()))
		assert.Equal(t, counterBefore, familyCounter(t, "sandbox_delete_responses"))
		assert.Equal(t, histBefore, familyHistogramCount(t, "sandbox_delete_duration_seconds"))
	})

	t.Run("snapshot before the sandbox is known records nothing", func(t *testing.T) {
		counterBefore := familyCounter(t, "sandbox_snapshot_total")
		histBefore := familyHistogramCount(t, "sandbox_snapshot_duration_seconds")
		_, apiErr := controller.CreateSnapshot(NewRequest(t, nil, models.NewSnapshotRequest{Name: "snap"}, map[string]string{
			"sandboxID": Namespace + "--missing-sandbox",
		}, user))
		require.NotNil(t, apiErr)
		assert.Equal(t, counterBefore, familyCounter(t, "sandbox_snapshot_total"))
		assert.Equal(t, histBefore, familyHistogramCount(t, "sandbox_snapshot_duration_seconds"))
	})
}

func TestHandlerPanicIsNotRecorded(t *testing.T) {
	before := familyCounter(t, "sandbox_pause_responses")
	histBefore := familyHistogramCount(t, "sandbox_pause_duration_seconds")
	sc := &Controller{}
	require.Panics(t, func() {
		_, _ = sc.PauseSandbox(NewRequest(t, nil, nil, map[string]string{"sandboxID": "x"}, adminTestUser()))
	})
	assert.Equal(t, before, familyCounter(t, "sandbox_pause_responses"))
	assert.Equal(t, histBefore, familyHistogramCount(t, "sandbox_pause_duration_seconds"))
}

func TestHandlerCallRecordsInterfaceOutcome(t *testing.T) {
	tests := []struct {
		name         string
		attempted    bool
		succeeded    bool
		wantSuccess  float64
		wantFailure  float64
		wantDuration uint64
	}{
		{name: "not attempted records nothing"},
		{name: "attempted failure records only the failure", attempted: true, wantFailure: 1},
		{name: "success records the result and the duration", attempted: true, succeeded: true, wantSuccess: 1, wantDuration: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_handler_responses"}, []string{"result"})
			duration := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "test_handler_duration_seconds"})

			call := newHandlerCall(time.Now(), duration, responses)
			if tt.attempted {
				call.attempt()
			}
			if tt.succeeded {
				call.succeed()
			}
			call.record()

			assert.Equal(t, tt.wantSuccess, counterValue(t, responses.WithLabelValues("success")))
			assert.Equal(t, tt.wantFailure, counterValue(t, responses.WithLabelValues("failure")))
			assert.Equal(t, tt.wantDuration, histogramSampleCount(t, duration))
		})
	}
}

func TestHandlerCallPanicRecordsNothing(t *testing.T) {
	responses := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_handler_panic_responses"}, []string{"result"})
	duration := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "test_handler_panic_duration_seconds"})

	func() {
		defer func() {
			require.Equal(t, "boom", recover())
		}()
		call := newHandlerCall(time.Now(), duration, responses)
		defer call.record()
		call.attempt()
		panic("boom")
	}()

	assert.Equal(t, 0.0, counterValue(t, responses.WithLabelValues("success")))
	assert.Equal(t, 0.0, counterValue(t, responses.WithLabelValues("failure")))
	assert.Equal(t, uint64(0), histogramSampleCount(t, duration))
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	var metric dto.Metric
	require.NoError(t, counter.Write(&metric))
	return metric.GetCounter().GetValue()
}

func histogramSampleCount(t *testing.T, histogram prometheus.Histogram) uint64 {
	t.Helper()
	var metric dto.Metric
	require.NoError(t, histogram.Write(&metric))
	return metric.GetHistogram().GetSampleCount()
}

func familyCounter(t *testing.T, name string) float64 {
	t.Helper()
	return labeledCounter(t, name, nil)
}

func familyHistogramCount(t *testing.T, name string) uint64 {
	t.Helper()
	return labeledHistogramCount(t, name, nil)
}

func labeledCounter(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	var sum float64
	for _, metric := range gatherMetrics(t, name, labels) {
		sum += metric.GetCounter().GetValue()
	}
	return sum
}

func labeledHistogramCount(t *testing.T, name string, labels map[string]string) uint64 {
	t.Helper()
	var sum uint64
	for _, metric := range gatherMetrics(t, name, labels) {
		sum += metric.GetHistogram().GetSampleCount()
	}
	return sum
}

func gatherMetrics(t *testing.T, name string, labels map[string]string) []*dto.Metric {
	t.Helper()
	families, err := metrics.Registry.Gather()
	require.NoError(t, err)
	var matched []*dto.Metric
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if labelsMatch(metric, labels) {
				matched = append(matched, metric)
			}
		}
	}
	return matched
}

func labelsMatch(metric *dto.Metric, labels map[string]string) bool {
	if len(labels) == 0 {
		return true
	}
	got := make(map[string]string, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		got[label.GetName()] = label.GetValue()
	}
	for key, value := range labels {
		if got[key] != value {
			return false
		}
	}
	return true
}
