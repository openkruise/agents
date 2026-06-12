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

package e2e

import (
	"context"
	"fmt"
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// controllerMetricsNamespace is the namespace where the agent-sandbox-controller
// is deployed (see config/default/kustomization.yaml: namespace=sandbox-system).
const controllerMetricsNamespace = "sandbox-system"

// controllerMetricsPodLabel is the label selector that matches the controller
// manager pod (see config/manager/manager.yaml).
const controllerMetricsPodLabel = "control-plane=sandbox-controller-manager"

// controllerMetricsPort is the metrics endpoint port exposed by the controller
// manager (see config/default/manager_metrics_patch.yaml: --metrics-bind-address=:8443).
const controllerMetricsPort = "8443"

// controllerMetricsScheme is the scheme used to access the metrics endpoint.
// The kubebuilder default deploys the metrics server with TLS enabled.
const controllerMetricsScheme = "https"

// fetchControllerMetrics retrieves the raw Prometheus metrics text from the
// agent-sandbox-controller's /metrics endpoint by proxying through the
// kube-apiserver Pod proxy. The first Ready pod matching the controller label
// selector is used.
func fetchControllerMetrics(ctx context.Context) (string, error) {
	pods, err := clientset.CoreV1().Pods(controllerMetricsNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: controllerMetricsPodLabel,
	})
	if err != nil {
		return "", fmt.Errorf("list controller pods: %w", err)
	}
	pod := pickReadyPod(pods.Items)
	if pod == nil {
		return "", fmt.Errorf("no ready controller pod found in namespace %q with selector %q",
			controllerMetricsNamespace, controllerMetricsPodLabel)
	}

	req := clientset.CoreV1().Pods(controllerMetricsNamespace).ProxyGet(
		controllerMetricsScheme, pod.Name, controllerMetricsPort, "/metrics", nil,
	)
	body, err := req.DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("proxy GET metrics on pod %q: %w", pod.Name, err)
	}
	return string(body), nil
}

// pickReadyPod returns the first pod whose Ready condition is True, or nil if
// none are ready.
func pickReadyPod(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return p
			}
		}
	}
	return nil
}

// metricSeriesExists reports whether the metrics body contains a series with
// the given metric name and label set. Labels are matched as a subset: the
// series may carry additional labels beyond those provided.
func metricSeriesExists(metricsBody, metricName string, labels map[string]string) bool {
	mfs, err := parseMetricFamilies(metricsBody)
	if err != nil {
		return false
	}
	mf, ok := mfs[metricName]
	if !ok {
		return false
	}
	for _, m := range mf.GetMetric() {
		if matchLabels(m.GetLabel(), labels) {
			return true
		}
	}
	return false
}

// metricHistogramHasObservation reports whether the named histogram has at least
// one observation matching the given label subset. The Prometheus text parser
// folds the `_count`, `_sum` and `_bucket` suffixes back under the bare
// histogram name, so callers must query by the bare metric name (without any
// suffix) for histograms.
func metricHistogramHasObservation(metricsBody, metricName string, labels map[string]string) (bool, error) {
	mfs, err := parseMetricFamilies(metricsBody)
	if err != nil {
		return false, err
	}
	mf, ok := mfs[metricName]
	if !ok || mf.GetType() != dto.MetricType_HISTOGRAM {
		return false, nil
	}
	for _, m := range mf.GetMetric() {
		if matchLabels(m.GetLabel(), labels) && m.GetHistogram().GetSampleCount() > 0 {
			return true, nil
		}
	}
	return false, nil
}

// getCounterValue returns the value of the counter (or untyped/gauge) series
// matching the given metric name and label set. It errors if zero or more than
// one matching series is found.
func getCounterValue(metricsBody, metricName string, labels map[string]string) (float64, error) {
	mfs, err := parseMetricFamilies(metricsBody)
	if err != nil {
		return 0, fmt.Errorf("parse metrics: %w", err)
	}
	mf, ok := mfs[metricName]
	if !ok {
		return 0, fmt.Errorf("metric %q not found", metricName)
	}

	matches := make([]*dto.Metric, 0)
	for _, m := range mf.GetMetric() {
		if matchLabels(m.GetLabel(), labels) {
			matches = append(matches, m)
		}
	}
	if len(matches) == 0 {
		return 0, fmt.Errorf("no series for %q matching labels %s", metricName, formatLabels(labels))
	}
	if len(matches) > 1 {
		return 0, fmt.Errorf("ambiguous match for %q labels %s: %d series", metricName, formatLabels(labels), len(matches))
	}
	return readMetricValue(matches[0])
}

// parseMetricFamilies parses the Prometheus text exposition format into a map
// of metric name to metric family.
func parseMetricFamilies(body string) (map[string]*dto.MetricFamily, error) {
	var parser expfmt.TextParser
	return parser.TextToMetricFamilies(strings.NewReader(body))
}

// matchLabels reports whether all wanted labels are present in actual with
// matching values. actual may carry extra labels.
func matchLabels(actual []*dto.LabelPair, wanted map[string]string) bool {
	if len(wanted) == 0 {
		return true
	}
	got := make(map[string]string, len(actual))
	for _, lp := range actual {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range wanted {
		if got[k] != v {
			return false
		}
	}
	return true
}

// readMetricValue extracts a numeric value from a metric. For counters and
// gauges the natural value is returned; for untyped metrics the untyped value
// is used. Histograms and summaries are not supported by this helper.
func readMetricValue(m *dto.Metric) (float64, error) {
	switch {
	case m.Counter != nil:
		return m.Counter.GetValue(), nil
	case m.Gauge != nil:
		return m.Gauge.GetValue(), nil
	case m.Untyped != nil:
		return m.Untyped.GetValue(), nil
	default:
		return 0, fmt.Errorf("metric is not a counter/gauge/untyped scalar")
	}
}

// formatLabels renders a label set as `{k1="v1",k2="v2"}` with keys sorted for
// deterministic error messages.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", k, labels[k])
	}
	b.WriteByte('}')
	return b.String()
}
