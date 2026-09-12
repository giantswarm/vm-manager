package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/host"
)

// bareHost is a host without any of the VM prerequisites.
func bareHost(t *testing.T) api.Services {
	t.Helper()
	runner := host.RunnerFunc(func(_ context.Context, name string, _ ...string) (string, error) {
		return "", errors.New(name + ": executable file not found in $PATH")
	})
	return api.Services{Host: host.New(host.Options{Runner: runner, Root: t.TempDir()})}
}

func TestProbesAndRoutes(t *testing.T) {
	svc := bareHost(t)
	srv, err := New(Config{Addr: "127.0.0.1:0"}, svc, api.NewMCPServer(svc, "test"), nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Content-Type")
	}
	status, _ := get("/healthz")
	assert.Equal(t, http.StatusOK, status)
	status, _ = get("/readyz")
	assert.Equal(t, http.StatusOK, status, "readiness is the listener, not the host prerequisites")
	status, ctype := get("/api/v1/host")
	assert.Equal(t, http.StatusOK, status, "an unready host is still described")
	assert.Equal(t, "application/json", ctype)
	status, ctype = get("/api/v1/does-not-exist")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "application/json", ctype, "REST 404s keep the JSON error body")

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"t"}}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(initialize))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the MCP endpoint is mounted at the default path")
}

func TestRunStopsOnContext(t *testing.T) {
	svc := bareHost(t)
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPPath: "/tools"}, svc, api.NewMCPServer(svc, "test"), quiet())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	cancel()
	require.NoError(t, <-done)
}
