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

package webhook

import (
	"context"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// fakeHandler is a minimal admission.Handler used to exercise the
// instrumented wrapper without requiring a real webhook stack.
type fakeHandler struct {
	resp admission.Response
}

func (f *fakeHandler) Handle(_ context.Context, _ admission.Request) admission.Response {
	return f.resp
}

func makeRequest(op admissionv1.Operation) admission.Request {
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: op}}
}

func TestInstrumentedHandler_RecordsAllowed(t *testing.T) {
	const path = "/test-allowed"
	resetCounter(admissionTotal, path, "CREATE", "true")
	resetCounter(admissionTotal, path, "CREATE", "false")

	h := newInstrumentedHandler(path, &fakeHandler{resp: admission.Allowed("ok")})
	resp := h.Handle(context.Background(), makeRequest(admissionv1.Create))
	if !resp.Allowed {
		t.Fatalf("inner handler said Allowed but wrapper returned Allowed=false")
	}

	gotAllowed := testutil.ToFloat64(admissionTotal.WithLabelValues(path, "CREATE", "true"))
	if gotAllowed != 1 {
		t.Errorf("admission_total{webhook=%q,operation=CREATE,allowed=true} = %v, want 1", path, gotAllowed)
	}
	gotDenied := testutil.ToFloat64(admissionTotal.WithLabelValues(path, "CREATE", "false"))
	if gotDenied != 0 {
		t.Errorf("denied counter unexpectedly incremented: got %v, want 0", gotDenied)
	}
}

func TestInstrumentedHandler_RecordsDenied(t *testing.T) {
	const path = "/test-denied"
	resetCounter(admissionTotal, path, "DELETE", "false")

	h := newInstrumentedHandler(path, &fakeHandler{resp: admission.Denied("nope")})
	resp := h.Handle(context.Background(), makeRequest(admissionv1.Delete))
	if resp.Allowed {
		t.Fatalf("inner handler said Denied but wrapper returned Allowed=true")
	}

	gotDenied := testutil.ToFloat64(admissionTotal.WithLabelValues(path, "DELETE", "false"))
	if gotDenied != 1 {
		t.Errorf("admission_total{webhook=%q,operation=DELETE,allowed=false} = %v, want 1", path, gotDenied)
	}
}

func TestInstrumentedHandler_RecordsErrored(t *testing.T) {
	const path = "/test-errored"
	resetCounter(admissionTotal, path, "UPDATE", "false")

	// admission.Errored returns Allowed=false with an HTTP status code, which
	// should be counted on the denied side of the counter.
	h := newInstrumentedHandler(path, &fakeHandler{resp: admission.Errored(http.StatusBadRequest, errBadDecode)})
	resp := h.Handle(context.Background(), makeRequest(admissionv1.Update))
	if resp.Allowed {
		t.Fatalf("Errored response unexpectedly Allowed=true")
	}
	got := testutil.ToFloat64(admissionTotal.WithLabelValues(path, "UPDATE", "false"))
	if got != 1 {
		t.Errorf("errored response counter = %v, want 1", got)
	}
}

func TestInstrumentedHandler_PassesThroughResponse(t *testing.T) {
	const path = "/test-passthrough"
	expected := admission.Allowed("custom-message")
	h := newInstrumentedHandler(path, &fakeHandler{resp: expected})
	got := h.Handle(context.Background(), makeRequest(admissionv1.Create))
	if got.Allowed != expected.Allowed || got.Result.Message != expected.Result.Message {
		t.Errorf("response was mutated by wrapper: got %+v, want %+v", got, expected)
	}
}

func TestInstrumentedHandler_DurationObserved(t *testing.T) {
	const path = "/test-duration"
	// Drop existing series so we can assert on a clean count.
	admissionDuration.DeleteLabelValues(path, "CREATE", "true")

	h := newInstrumentedHandler(path, &fakeHandler{resp: admission.Allowed("ok")})
	for i := 0; i < 3; i++ {
		h.Handle(context.Background(), makeRequest(admissionv1.Create))
	}

	hist, err := admissionDuration.GetMetricWithLabelValues(path, "CREATE", "true")
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues failed: %v", err)
	}
	count := histogramVecSampleCount(t, hist.(prometheus.Histogram))
	if count != 3 {
		t.Errorf("duration histogram sample count = %d, want 3", count)
	}
}

// resetCounter zeroes a single label series on a CounterVec by deleting it.
// Subsequent WithLabelValues calls return a fresh zero-valued counter.
func resetCounter(vec *prometheus.CounterVec, labels ...string) {
	vec.DeleteLabelValues(labels...)
}

// histogramVecSampleCount returns the observation count from a Histogram by
// reading its DTO representation.
func histogramVecSampleCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	dtoMetric := &dto.Metric{}
	if err := h.Write(dtoMetric); err != nil {
		t.Fatalf("histogram Write failed: %v", err)
	}
	return dtoMetric.GetHistogram().GetSampleCount()
}

// errBadDecode is a sentinel used in error-path tests.
var errBadDecode = &decodeError{msg: "bad decode"}

type decodeError struct{ msg string }

func (d *decodeError) Error() string { return d.msg }
