package metrics

import "k8s.io/component-base/metrics"

// RequestsTotalForTest exposes one counter child so external tests can read it.
func RequestsTotalForTest(method, operation, code string) metrics.CounterMetric {
	return requestsTotal.WithLabelValues(method, operation, code)
}

// RequestDurationForTest exposes one histogram child so external tests can read it.
func RequestDurationForTest(method, operation string) metrics.ObserverMetric {
	return requestDuration.WithLabelValues(method, operation)
}
