package metrics_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/metrics"
	"github.com/stretchr/testify/require"
	"k8s.io/component-base/metrics/testutil"
)

var errBoom = errors.New("boom")

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errBoom
}

const (
	testProjectID   = "1841af90-a4f6-4412-8b23-b7035a6c72ae"
	testPartitionID = "2480b2f8-d63a-401e-90ff-0d79b5b3e007"
	// Route templates below come from github.com/crusoecloud/client-go swagger/v1alpha5,
	// prefixed with the /v1alpha5 base path the CCM is configured with.
	opListInstances  = "/v1alpha5/projects/{id}/compute/vms/instances"
	opGetIBPartition = "/v1alpha5/projects/{id}/networking/ib-partitions/{id}"
)

func TestOperationFromPath(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		// The two routes the CCM actually calls (VMsApi.ListInstances, IBPartitionsApi.GetIBPartition).
		"/v1alpha5/projects/" + testProjectID + "/compute/vms/instances":                       opListInstances,
		"/v1alpha5/projects/" + testProjectID + "/networking/ib-partitions/" + testPartitionID: opGetIBPartition,
		// Uppercase UUIDs and purely numeric segments are identifiers too.
		"/v1alpha5/projects/" + strings.ToUpper(testProjectID) + "/compute/vms/instances": opListInstances,
		"/v1alpha5/projects/12345/compute/vms/instances":                                  opListInstances,
		// Deepest real route in client-go (9 segments after the prefix) survives the segment cap.
		"/v1alpha5/projects/" + testProjectID + "/kubernetes/clusters/" + testPartitionID +
			"/autoclusters/vms/" + testPartitionID + "/remediate": "" +
			"/v1alpha5/projects/{id}/kubernetes/clusters/{id}/autoclusters/vms/{id}/remediate",
		// Non-identifier segments are preserved verbatim; near-UUIDs are not collapsed.
		"/v1alpha5/locations":                      "/v1alpha5/locations",
		"/v1alpha5/projects/not-a-uuid/compute":    "/v1alpha5/projects/not-a-uuid/compute",
		"/v1alpha5/projects/1841af90-a4f6/compute": "/v1alpha5/projects/1841af90-a4f6/compute",
		// Leading/trailing slashes and empty paths normalize.
		"":                                 "/",
		"/":                                "/",
		"v1alpha5/projects/x/compute/vms/": "/v1alpha5/projects/x/compute/vms",
		// Pathological depth is truncated to the cap.
		"/a/b/c/d/e/f/g/h/i/j/k/l/m/n": "/a/b/c/d/e/f/g/h/i/j/k/l",
	}
	for in, want := range cases {
		require.Equal(t, want, metrics.OperationFromPath(in), "input %q", in)
	}
}

func TestOperationFromPathIgnoresQuery(t *testing.T) {
	t.Parallel()

	// ListInstances filters by ids= / names= in the query string; RoundTrip only
	// passes URL.Path, so query values never reach the label. Confirm the
	// normalizer itself does not care about a query either.
	u, err := url.Parse("https://api.crusoecloud.com/v1alpha5/projects/" + testProjectID +
		"/compute/vms/instances?ids=" + testPartitionID + "&names=node1")
	require.NoError(t, err)
	require.Equal(t, opListInstances, metrics.OperationFromPath(u.Path))
}

func TestTransportRecordsStatusCode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") == "1" {
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: metrics.NewTransport(nil)}
	const path = "/v1alpha5/projects/" + testProjectID + "/compute/vms/instances"

	for _, q := range []string{"", "?fail=1", "?fail=1"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path+q, http.NoBody)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	ok, err := testutil.GetCounterMetricValue(
		metrics.RequestsTotalForTest("GET", opListInstances, "200"))
	require.NoError(t, err)
	require.InDelta(t, 1, ok, 0)

	throttled, err := testutil.GetCounterMetricValue(
		metrics.RequestsTotalForTest("GET", opListInstances, "429"))
	require.NoError(t, err)
	require.InDelta(t, 2, throttled, 0)

	count, err := testutil.GetHistogramMetricCount(
		metrics.RequestDurationForTest("GET", opListInstances))
	require.NoError(t, err)
	require.Equal(t, uint64(3), count)
}

func TestTransportRecordsTransportError(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: metrics.NewTransport(failingTransport{})}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete,
		"http://example.invalid/v1alpha5/projects/"+testProjectID+"/networking/ib-partitions/"+testPartitionID,
		http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req) //nolint:bodyclose // no response is returned on a transport error
	require.ErrorIs(t, err, errBoom)
	require.Nil(t, resp)

	v, err := testutil.GetCounterMetricValue(
		metrics.RequestsTotalForTest("DELETE", opGetIBPartition, "<error>"))
	require.NoError(t, err)
	require.InDelta(t, 1, v, 0)
}
