package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/host"
	"github.com/giantswarm/vm-manager/internal/server"
)

// fakeTools answers the version probes; the empty root makes every device
// and firmware probe report absent, which is what a laptop without the VM
// stack looks like.
func fakeTools(_ context.Context, name string, _ ...string) (string, error) {
	switch name {
	case host.QEMUBinary:
		return "QEMU emulator version 11.1.1\n", nil
	case "systemctl":
		return "systemd 261 (261.3-1-arch)\n", nil
	default:
		return "", errors.New(name + ": executable file not found in $PATH")
	}
}

func newTestServer(t *testing.T) (*httptest.Server, api.Services) {
	t.Helper()
	svc := api.Services{Host: host.New(host.Options{Runner: host.RunnerFunc(fakeTools), Root: t.TempDir()})}
	srv, err := server.New(server.Config{Addr: "127.0.0.1:0", MCPPath: "/mcp"}, svc, api.NewMCPServer(svc, "test"), nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc
}

// TestMCPContract drives the assembled server the way muster does: an MCP
// client over streamable HTTP initializes, lists the tools and calls
// get_host; the result must be the REST body byte for byte.
func TestMCPContract(t *testing.T) {
	ts, _ := newTestServer(t)
	ctx := context.Background()

	c, err := mcpclient.NewStreamableHttpClient(ts.URL + "/mcp")
	require.NoError(t, err)
	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() { _ = c.Close() })

	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "contract-test", Version: "test"}
	initRes, err := c.Initialize(ctx, initReq)
	require.NoError(t, err)
	assert.Equal(t, "vm-manager", initRes.ServerInfo.Name)
	assert.Equal(t, "test", initRes.ServerInfo.Version)
	assert.Contains(t, initRes.Instructions, "get_host first")

	listed, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	assert.Equal(t, api.ToolNames(), names, "exactly the declared tools, in order")
	require.Len(t, listed.Tools, 1)
	getHost := listed.Tools[0]
	assert.Equal(t, api.ToolGetHost, getHost.Name)
	assert.True(t, *getHost.Annotations.ReadOnlyHint)
	assert.False(t, *getHost.Annotations.DestructiveHint)
	assert.True(t, *getHost.Annotations.IdempotentHint)
	assert.False(t, *getHost.Annotations.OpenWorldHint)
	assert.Regexp(t, `^Read-only\.`, getHost.Description)

	var call mcp.CallToolRequest
	call.Params.Name = api.ToolGetHost
	res, err := c.CallTool(ctx, call)
	require.NoError(t, err)
	require.False(t, res.IsError, "%+v", res.Content)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "tool results are JSON text")

	var viaMCP host.Info
	require.NoError(t, json.Unmarshal([]byte(text.Text), &viaMCP))
	assert.Equal(t, host.Tool{Found: true, Version: "11.1.1"}, viaMCP.QEMU)
	assert.False(t, viaMCP.Ready)
	assert.Contains(t, viaMCP.Missing, host.KVMDevice)
	assert.Contains(t, viaMCP.Missing, host.SwtpmBinary)

	resp, err := http.Get(ts.URL + api.Prefix + "/host")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var viaREST host.Info
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&viaREST))
	assert.Equal(t, viaREST, viaMCP, "MCP and REST expose the same operation")

	call.Params.Name = "no_such_tool"
	_, err = c.CallTool(ctx, call)
	require.Error(t, err, "unknown tools are a protocol error, not a result")
}
