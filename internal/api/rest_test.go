package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/host"
)

func newRESTServer(t *testing.T) *httptest.Server {
	t.Helper()
	runner := host.RunnerFunc(func(_ context.Context, name string, _ ...string) (string, error) {
		return "", errors.New(name + ": executable file not found in $PATH")
	})
	svc := Services{Host: host.New(host.Options{Runner: runner, Root: t.TempDir()})}
	mux := http.NewServeMux()
	NewREST(svc, nil).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, ts *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

func TestGetHost(t *testing.T) {
	ts := newRESTServer(t)
	resp, body := get(t, ts, Prefix+"/host")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var info host.Info
	require.NoError(t, json.Unmarshal(body, &info))
	assert.NotEmpty(t, info.Hostname)
	assert.False(t, info.Ready)
	assert.NotEmpty(t, info.Missing)
	assert.Equal(t, host.KVMDevice, info.KVM.Path)
	assert.Contains(t, string(body), `"storageProviders": []`, "lists are never null")
}

func TestOpenAPI(t *testing.T) {
	ts := newRESTServer(t)
	resp, body := get(t, ts, Prefix+"/openapi.yaml")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/yaml", resp.Header.Get("Content-Type"))
	assert.Contains(t, string(body), "openapi: 3.1.0")
	assert.Contains(t, string(body), "/host:", "every REST route is documented")
}

func TestUnknownRouteIsJSON(t *testing.T) {
	ts := newRESTServer(t)
	resp, body := get(t, ts, Prefix+"/vms/nope")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	var e errorBody
	require.NoError(t, json.Unmarshal(body, &e))
	assert.Equal(t, "not_found", e.Error.Code)
	assert.Contains(t, e.Error.Message, "GET /api/v1/vms/nope")
}

func TestStatusFor(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: vm x", apierr.ErrNotFound), http.StatusNotFound, "not_found"},
		{fmt.Errorf("%w: name required", apierr.ErrInvalid), http.StatusBadRequest, "invalid_request"},
		{fmt.Errorf("%w: vm running", apierr.ErrConflict), http.StatusConflict, "conflict"},
		{fmt.Errorf("%w: no kvm", apierr.ErrUnsupported), http.StatusNotImplemented, "unsupported"},
		{errors.New("qmp: connection reset"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			status, code := statusFor(tt.err)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.code, code)
			res := errResult(tt.err)
			assert.True(t, res.IsError)
			assert.Contains(t, fmt.Sprint(res.Content[0]), tt.code+": ", "MCP errors carry the REST code")
		})
	}
}
