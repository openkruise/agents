package host

type MetricsProvider interface {
	GetMetrics() (*Metrics, error)
}