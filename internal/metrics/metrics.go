// Package metrics instruments the CCM's outbound calls to the Crusoe Cloud API.
//
// The metrics are registered in the component-base legacy registry, which the
// cloud-controller-manager framework already serves on its /metrics endpoint
// alongside the standard workqueue and rest_client metrics.
package metrics

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	namespace = "crusoe_ccm"
	subsystem = "cloud_api"

	// codeTransportError mirrors the client-go convention for requests that
	// never produced an HTTP response (DNS failure, connection refused, timeout).
	codeTransportError = "<error>"
	// maxPathSegments bounds the operation label so an unexpected URL shape
	// cannot explode cardinality. The deepest v1alpha5 route in client-go is
	// 9 segments after the /v1alpha5 prefix, so 12 keeps every real route intact.
	maxPathSegments = 12
)

//nolint:gochecknoglobals // prometheus metrics are process-wide by design
var (
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	digitsRe = regexp.MustCompile(`^\d+$`)

	registerOnce sync.Once

	requestsTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "requests_total",
			Help:           "Number of HTTP requests from the CCM to the Crusoe Cloud API, by method, operation, and code.",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"method", "operation", "code"},
	)

	requestDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Namespace:      namespace,
			Subsystem:      subsystem,
			Name:           "request_duration_seconds",
			Help:           "Latency of HTTP requests the CCM made to the Crusoe Cloud API, by method and operation.",
			Buckets:        []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"method", "operation"},
	)
)

// Register adds the Crusoe Cloud API metrics to the legacy registry. It is
// safe to call more than once; only the first call registers.
func Register() {
	registerOnce.Do(func() {
		legacyregistry.MustRegister(requestsTotal)
		legacyregistry.MustRegister(requestDuration)
	})
}

// Transport is an http.RoundTripper that records a counter and a latency
// histogram for every request it forwards.
type Transport struct {
	next http.RoundTripper
}

// NewTransport wraps r so every request through it is recorded. A nil r falls
// back to http.DefaultTransport. Calling NewTransport also registers the metrics.
func NewTransport(r http.RoundTripper) *Transport {
	if r == nil {
		r = http.DefaultTransport
	}
	Register()

	return &Transport{next: r}
}

// RoundTrip forwards the request and records its latency and response code.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	operation := OperationFromPath(r.URL.Path)
	start := time.Now()

	resp, err := t.next.RoundTrip(r)

	requestDuration.WithLabelValues(r.Method, operation).Observe(time.Since(start).Seconds())
	requestsTotal.WithLabelValues(r.Method, operation, codeLabel(resp, err)).Inc()

	//nolint:wrapcheck // transport errors must be forwarded unchanged to the caller.
	return resp, err
}

func codeLabel(resp *http.Response, err error) string {
	if err != nil || resp == nil {
		return codeTransportError
	}

	return strconv.Itoa(resp.StatusCode)
}

// OperationFromPath turns a concrete request path into a low-cardinality
// operation label by replacing identifier segments with "{id}". For example
// "/v1alpha5/projects/6b1e.../compute/vms/instances" becomes
// "/v1alpha5/projects/{id}/compute/vms/instances".
func OperationFromPath(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) > maxPathSegments {
		segments = segments[:maxPathSegments]
	}
	for i, s := range segments {
		if uuidRe.MatchString(s) || digitsRe.MatchString(s) {
			segments[i] = "{id}"
		}
	}

	return "/" + strings.Join(segments, "/")
}
