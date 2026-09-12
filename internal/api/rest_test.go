package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/host"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// errBody is the REST error envelope.
type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newRESTServer(t *testing.T) (*httptest.Server, api.Services) {
	t.Helper()
	svc := newServices(t)
	mux := http.NewServeMux()
	api.NewREST(svc, nil).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, svc
}

// do sends a request with an optional JSON body and returns the status and
// body.
func do(t *testing.T, ts *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		switch b := body.(type) {
		case string:
			buf.WriteString(b)
		default:
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
	}
	req, err := http.NewRequest(method, ts.URL+api.Prefix+path, &buf)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out
}

// expectError asserts the JSON error envelope with the given status and code.
func expectError(t *testing.T, status int, body []byte, wantStatus int, wantCode string) errBody {
	t.Helper()
	var e errBody
	require.Equal(t, wantStatus, status, string(body))
	require.NoError(t, json.Unmarshal(body, &e), string(body))
	assert.Equal(t, wantCode, e.Error.Code)
	assert.NotEmpty(t, e.Error.Message)
	return e
}

func TestGetHost(t *testing.T) {
	ts, _ := newRESTServer(t)
	status, body := do(t, ts, http.MethodGet, "/host", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var info host.Info
	require.NoError(t, json.Unmarshal(body, &info))
	assert.NotEmpty(t, info.Hostname)
	assert.False(t, info.Ready)
	assert.NotEmpty(t, info.Missing)
	assert.Contains(t, string(body), `"storageProviders": []`, "lists are never null")
}

func TestOpenAPI(t *testing.T) {
	ts, _ := newRESTServer(t)
	status, body := do(t, ts, http.MethodGet, "/openapi.yaml", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(body), "openapi: 3.1.0")
	for _, route := range []string{
		"/host:", "/images:", "/images/{ref}:", "/networks:", "/networks/{name}:", "/vms:", "/vms/{id}:",
		"/vms/{id}/start:", "/vms/{id}/stop:", "/vms/{id}/reboot:", "/vms/{id}/exec:", "/vms/{id}/forward:",
		"/vms/{id}/console:", "/vms/{id}/attestation:", "/vms/{id}/metrics:", "/vms/{id}/report:",
	} {
		assert.Contains(t, string(body), "  "+route, "every REST route is documented")
	}
	for _, schema := range []string{"VM:", "Network:", "Image:", "ExecResult:", "Error:", "Attestation:"} {
		assert.Contains(t, string(body), "    "+schema)
	}
	for _, code := range []string{"timeout", "vm_failed"} {
		assert.Contains(t, string(body), code, "the milestone error codes are documented")
	}
}

func TestUnknownRouteIsJSON(t *testing.T) {
	ts, _ := newRESTServer(t)
	status, body := do(t, ts, http.MethodGet, "/nope", nil)
	e := expectError(t, status, body, http.StatusNotFound, "not_found")
	assert.Contains(t, e.Error.Message, "GET /api/v1/nope")
}

// TestVMLifecycleREST walks networks and VMs over REST and checks the error
// mapping: 404 for unknown resources, 409 for a network in use and a
// lifecycle conflict, 400 for invalid specs and bodies.
func TestVMLifecycleREST(t *testing.T) {
	ts, svc := newRESTServer(t)

	status, body := do(t, ts, http.MethodGet, "/images/"+testImageRef, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = do(t, ts, http.MethodGet, "/images/nope", nil)
	expectError(t, status, body, http.StatusNotFound, "not_found")

	status, body = do(t, ts, http.MethodPost, "/networks", api.CreateNetworkRequest{Name: testNetwork, CIDR: testCIDR})
	require.Equal(t, http.StatusCreated, status, string(body))
	var n vm.NetworkInfo
	require.NoError(t, json.Unmarshal(body, &n))
	assert.Equal(t, "10.10.0.1", n.Gateway)
	status, body = do(t, ts, http.MethodPost, "/networks", api.CreateNetworkRequest{Name: testNetwork, CIDR: testCIDR})
	expectError(t, status, body, http.StatusConflict, "conflict")
	status, body = do(t, ts, http.MethodPost, "/networks", `{"name": "x", "cidr": "10.1.0.0/24", "bogus": 1}`)
	expectError(t, status, body, http.StatusBadRequest, "invalid_request")
	status, body = do(t, ts, http.MethodPost, "/networks", `{"name": `)
	expectError(t, status, body, http.StatusBadRequest, "invalid_request")
	status, body = do(t, ts, http.MethodGet, "/networks/nope", nil)
	expectError(t, status, body, http.StatusNotFound, "not_found")

	status, body = do(t, ts, http.MethodPost, "/vms", api.CreateVMRequest{Name: "node-1", Network: testNetwork, CPUs: 9999, WaitFor: "none"})
	e := expectError(t, status, body, http.StatusBadRequest, "invalid_request")
	assert.Contains(t, e.Error.Message, "cpus")
	status, body = do(t, ts, http.MethodPost, "/vms", api.CreateVMRequest{Name: "node-1", Network: testNetwork, WaitFor: "later"})
	expectError(t, status, body, http.StatusBadRequest, "invalid_request")
	status, body = do(t, ts, http.MethodPost, "/vms", api.CreateVMRequest{Name: "node-1", Network: "nope", WaitFor: "none"})
	expectError(t, status, body, http.StatusBadRequest, "invalid_request")

	status, body = do(t, ts, http.MethodPost, "/vms", api.CreateVMRequest{Name: "node-1", Network: testNetwork, WaitFor: "none", DiskGiB: 10})
	require.Equal(t, http.StatusCreated, status, string(body))
	var v vm.VM
	require.NoError(t, json.Unmarshal(body, &v))
	assert.Equal(t, vm.StateInstalling, v.State)
	assert.Equal(t, 10, v.DiskGiB)
	assert.Equal(t, api.DefaultMemoryMiB, v.MemoryMiB)

	status, body = do(t, ts, http.MethodGet, "/vms", nil)
	require.Equal(t, http.StatusOK, status)
	var vms []vm.VM
	require.NoError(t, json.Unmarshal(body, &vms))
	require.Len(t, vms, 1)

	status, body = do(t, ts, http.MethodGet, "/vms/"+v.ID+"/console?lines=3", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var console api.ConsoleResponse
	require.NoError(t, json.Unmarshal(body, &console))
	assert.Equal(t, api.ConsoleResponse{ID: v.ID, Lines: 3}, console)
	status, body = do(t, ts, http.MethodGet, "/vms/"+v.ID+"/console?lines=many", nil)
	expectError(t, status, body, http.StatusBadRequest, "invalid_request")
	status, body = do(t, ts, http.MethodGet, "/vms/"+v.ID+"/attestation", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = do(t, ts, http.MethodGet, "/vms/"+v.ID+"/metrics", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var m api.MetricsResponse
	require.NoError(t, json.Unmarshal(body, &m))
	assert.Equal(t, string(vm.StateInstalling), m.Host.State)
	assert.Nil(t, m.Guest)
	assert.Contains(t, string(body), `"guest": null`, "the guest section is explicit")
	status, body = do(t, ts, http.MethodGet, "/vms/"+v.ID+"/report", nil)
	expectError(t, status, body, http.StatusNotFound, "not_found")
	require.NoError(t, svc.VM.StoreReport(context.Background(), v.ID, []byte(`{"metrics":[{"name":"io.systemd.Manager.UnitsTotal","value":7}]}`)))
	status, body = do(t, ts, http.MethodGet, "/vms/"+v.ID+"/report", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.JSONEq(t, `{"metrics":[{"name":"io.systemd.Manager.UnitsTotal","value":7}]}`, string(body), "the upload is served in full")
	status, body = do(t, ts, http.MethodGet, "/vms/"+v.ID+"/metrics", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &m))
	require.NotNil(t, m.Guest)
	assert.Equal(t, 1, m.Guest.SeriesExported)
	assert.Equal(t, api.Prefix+"/vms/"+v.ID+"/report", m.RawReportURL)

	status, body = do(t, ts, http.MethodPost, "/vms/"+v.ID+"/start", nil)
	expectError(t, status, body, http.StatusConflict, "conflict")
	status, body = do(t, ts, http.MethodPost, "/vms/"+v.ID+"/exec", api.ExecRequest{})
	expectError(t, status, body, http.StatusBadRequest, "invalid_request")
	status, body = do(t, ts, http.MethodPost, "/vms/"+v.ID+"/forward", api.ForwardRequest{Port: 0})
	expectError(t, status, body, http.StatusBadRequest, "invalid_request")
	status, body = do(t, ts, http.MethodPost, "/vms/"+v.ID+"/forward", api.ForwardRequest{Port: 22})
	require.Equal(t, http.StatusOK, status, string(body))
	var fwd api.ForwardResponse
	require.NoError(t, json.Unmarshal(body, &fwd))
	assert.Equal(t, 22, fwd.Port)
	assert.Contains(t, fwd.Address, "127.0.0.1:")

	status, body = do(t, ts, http.MethodDelete, "/networks/"+testNetwork, nil)
	e = expectError(t, status, body, http.StatusConflict, "conflict")
	assert.Contains(t, e.Error.Message, "node-1")

	status, body = do(t, ts, http.MethodGet, "/vms/nope", nil)
	expectError(t, status, body, http.StatusNotFound, "not_found")
	status, body = do(t, ts, http.MethodDelete, "/vms/nope", nil)
	expectError(t, status, body, http.StatusNotFound, "not_found")

	status, body = do(t, ts, http.MethodDelete, "/vms/"+v.ID, nil)
	require.Equal(t, http.StatusNoContent, status, string(body))
	assert.Empty(t, body)
	status, body = do(t, ts, http.MethodDelete, "/networks/"+testNetwork, nil)
	require.Equal(t, http.StatusNoContent, status, string(body))
	status, body = do(t, ts, http.MethodGet, "/networks", nil)
	require.Equal(t, http.StatusOK, status)
	assert.JSONEq(t, "[]", string(body))
}
