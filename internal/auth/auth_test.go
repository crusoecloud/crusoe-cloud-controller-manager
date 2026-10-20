package auth_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:paralleltest // reads and restores the process-wide http.DefaultClient
func TestNewCrusoeClientLeavesDefaultClientUnsigned(t *testing.T) {
	original := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = original })

	headers := make(chan http.Header, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{}`)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)

	secret := base64.RawURLEncoding.EncodeToString([]byte("test-secret"))
	client := auth.NewCrusoeClient(srv.URL, "test-key", secret, "test-agent")

	assert.Equal(t, original, http.DefaultClient.Transport, "http.DefaultClient.Transport was replaced")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	unsigned := <-headers
	assert.Empty(t, unsigned.Get("Authorization"), "http.DefaultClient request carried Crusoe credentials")
	assert.Empty(t, unsigned.Get("X-Crusoe-Timestamp"))

	_, resp, err = client.ProjectsApi.GetProject(context.Background(), "p")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	signed := <-headers
	require.True(t, strings.HasPrefix(signed.Get("Authorization"), "Bearer 1.0:test-key:"),
		"Crusoe client request was not signed")
	require.NotEmpty(t, signed.Get("X-Crusoe-Timestamp"))
}
