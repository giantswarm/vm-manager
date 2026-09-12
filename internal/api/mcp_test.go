package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/host"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/server"
	"github.com/giantswarm/vm-manager/internal/vm"
	"github.com/giantswarm/vm-manager/internal/vm/vmtest"
)

const (
	testImageRef = "giantswarm-vm-base_0.1.0"
	testNetwork  = "lan"
	testCIDR     = "10.10.0.0/24"
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

// newServices wires the real image catalog and VM service to the vmtest
// fakes: one image, no network yet.
func newServices(t *testing.T) api.Services {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	imageDir := t.TempDir()
	for _, f := range []string{testImageRef + ".efi", testImageRef + ".raw", "OVMF_CODE.fd", "OVMF_VARS.fd"} {
		require.NoError(t, os.WriteFile(filepath.Join(imageDir, f), []byte(f), 0o600))
	}
	sums := filepath.Join(imageDir, images.SysupdateDirName, images.KubernetesComponent)
	require.NoError(t, os.MkdirAll(sums, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(sums, "SHA256SUMS"), []byte("00  kubernetes_1.32.0.raw\n"), 0o600))
	catalog, err := images.Load(imageDir, quiet)
	require.NoError(t, err)

	d := vmtest.NewDeps(t.TempDir(), time.Now())
	svc, err := vm.New(vm.Options{
		StateDir:         t.TempDir(),
		Images:           catalog,
		Storage:          d.Storage,
		TPM:              d.TPM,
		Runtime:          d.Runtime,
		Networks:         d.Networks,
		Notify:           d.Notifier,
		OVMFCode:         filepath.Join(imageDir, "OVMF_CODE.fd"),
		OVMFVarsTemplate: filepath.Join(imageDir, "OVMF_VARS.fd"),
		Region:           "host1",
		Logger:           quiet,
		Clock:            d.Clock,
	})
	require.NoError(t, err)
	require.NoError(t, svc.Load(context.Background()))
	t.Cleanup(func() { require.NoError(t, svc.Close(context.Background())) })
	return api.Services{
		Host:   host.New(host.Options{Runner: host.RunnerFunc(fakeTools), Root: t.TempDir()}),
		VM:     svc,
		Images: catalog,
	}
}

func newTestServer(t *testing.T) (*httptest.Server, api.Services) {
	t.Helper()
	svc := newServices(t)
	srv, err := server.New(server.Config{Addr: "127.0.0.1:0", MCPPath: "/mcp"}, svc, api.NewMCPServer(svc, "test"), nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc
}

// mcpSession is an initialized client with a call helper.
type mcpSession struct {
	t *testing.T
	c *mcpclient.Client
}

func newMCPSession(t *testing.T, url string) (*mcpSession, *mcp.InitializeResult) {
	t.Helper()
	ctx := context.Background()
	c, err := mcpclient.NewStreamableHttpClient(url + "/mcp")
	require.NoError(t, err)
	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() { _ = c.Close() })
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "contract-test", Version: "test"}
	initRes, err := c.Initialize(ctx, initReq)
	require.NoError(t, err)
	return &mcpSession{t: t, c: c}, initRes
}

// call invokes a tool and decodes its JSON text result into out.
func (s *mcpSession) call(name string, args map[string]any, out any) {
	s.t.Helper()
	res := s.callRaw(name, args)
	require.False(s.t, res.IsError, "%s: %+v", name, res.Content)
	require.Len(s.t, res.Content, 1)
	text, ok := res.Content[0].(mcp.TextContent)
	require.True(s.t, ok, "tool results are JSON text")
	if out != nil {
		require.NoError(s.t, json.Unmarshal([]byte(text.Text), out), text.Text)
	}
}

func (s *mcpSession) callRaw(name string, args map[string]any) *mcp.CallToolResult {
	s.t.Helper()
	var call mcp.CallToolRequest
	call.Params.Name = name
	call.Params.Arguments = args
	res, err := s.c.CallTool(context.Background(), call)
	require.NoError(s.t, err)
	return res
}

// errText returns the text of a failed tool call.
func (s *mcpSession) errText(name string, args map[string]any) string {
	s.t.Helper()
	res := s.callRaw(name, args)
	require.True(s.t, res.IsError, "%s should fail: %+v", name, res.Content)
	text, ok := res.Content[0].(mcp.TextContent)
	require.True(s.t, ok)
	return text.Text
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url) // #nosec G107 -- test server URL.
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if out != nil && resp.StatusCode < 300 {
		require.NoError(t, json.Unmarshal(body, out), string(body))
	}
	return resp.StatusCode
}

// TestMCPContract drives the assembled server the way muster does: an MCP
// client over streamable HTTP initializes, lists the tools and walks the
// lifecycle create_network -> create_vm -> get_vm -> delete_vm ->
// delete_network against the fake runtime; every result must match the REST
// body of the same operation.
func TestMCPContract(t *testing.T) {
	ts, _ := newTestServer(t)
	s, initRes := newMCPSession(t, ts.URL)
	ctx := context.Background()

	assert.Equal(t, "vm-manager", initRes.ServerInfo.Name)
	assert.Equal(t, "test", initRes.ServerInfo.Version)
	assert.Contains(t, initRes.Instructions, "get_host first")
	assert.Contains(t, initRes.Instructions, "Ignition JSON")
	assert.Contains(t, initRes.Instructions, "create_network")

	listed, err := s.c.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		a := tool.Annotations
		require.NotNil(t, a.ReadOnlyHint, tool.Name)
		require.NotNil(t, a.DestructiveHint, tool.Name)
		require.NotNil(t, a.IdempotentHint, tool.Name)
		require.NotNil(t, a.OpenWorldHint, tool.Name)
		if *a.ReadOnlyHint {
			assert.Regexp(t, `^Read-only\.`, tool.Description, tool.Name)
			assert.False(t, *a.DestructiveHint, tool.Name)
		} else {
			assert.Regexp(t, `^WRITES( \(destructive\))?:`, tool.Description, tool.Name)
		}
		if *a.DestructiveHint {
			assert.Regexp(t, `^WRITES \(destructive\):`, tool.Description, tool.Name)
		}
	}
	assert.ElementsMatch(t, api.ToolNames(), names, "exactly the declared tools (tools/list sorts by name)")
	assert.ElementsMatch(t, []string{
		"get_host", "list_images", "get_image", "list_networks", "get_network", "list_vms", "get_vm",
		"get_vm_console", "get_vm_metrics", "get_vm_attestation", "create_network", "delete_network",
		"create_vm", "start_vm", "stop_vm", "reboot_vm", "delete_vm", "exec_vm", "forward_port",
	}, names, "the v1 surface from docs/design.md")

	// Read-only: host and images.
	var viaMCP, viaREST host.Info
	s.call(api.ToolGetHost, nil, &viaMCP)
	assert.Equal(t, host.Tool{Found: true, Version: "11.1.1"}, viaMCP.QEMU)
	assert.False(t, viaMCP.Ready)
	assert.Contains(t, viaMCP.Missing, host.KVMDevice)
	require.Equal(t, http.StatusOK, getJSON(t, ts.URL+api.Prefix+"/host", &viaREST))
	assert.Equal(t, viaREST, viaMCP, "MCP and REST expose the same operation")

	var imgs, restImgs []images.Image
	s.call(api.ToolListImages, nil, &imgs)
	require.Len(t, imgs, 1)
	assert.Equal(t, testImageRef, imgs[0].Ref())
	assert.Equal(t, []string{"1.32.0"}, imgs[0].KubernetesVersions)
	require.Equal(t, http.StatusOK, getJSON(t, ts.URL+api.Prefix+"/images", &restImgs))
	assert.Equal(t, imgs, restImgs)
	var img images.Image
	s.call(api.ToolGetImage, map[string]any{"ref": "giantswarm-vm-base"}, &img)
	assert.Equal(t, imgs[0], img, "a bare id resolves to the newest version")
	assert.Regexp(t, `^not_found: `, s.errText(api.ToolGetImage, map[string]any{"ref": "nope"}))
	assert.Regexp(t, `^invalid_request: `, s.errText(api.ToolGetImage, nil))

	// Network lifecycle.
	var nets []vm.NetworkInfo
	s.call(api.ToolListNetworks, nil, &nets)
	assert.Empty(t, nets)
	var n vm.NetworkInfo
	s.call(api.ToolCreateNetwork, map[string]any{"name": testNetwork, "cidr": testCIDR, "dns_search_domain": "lab.internal"}, &n)
	assert.Equal(t, testNetwork, n.Spec.Name)
	assert.Equal(t, "10.10.0.1", n.Gateway)
	assert.Equal(t, "lab.internal", n.Spec.DNSSearchDomain)
	assert.True(t, n.Spec.EnableIMDS, "the IMDS is always on")
	assert.Empty(t, n.Leases)
	s.call(api.ToolListNetworks, nil, &nets)
	require.Len(t, nets, 1)
	var restNet vm.NetworkInfo
	require.Equal(t, http.StatusOK, getJSON(t, ts.URL+api.Prefix+"/networks/"+testNetwork, &restNet))
	assert.Equal(t, n, restNet)
	assert.Regexp(t, `^conflict: `, s.errText(api.ToolCreateNetwork, map[string]any{"name": testNetwork, "cidr": testCIDR}))

	// VM lifecycle with wait_for none: the installer is started and the
	// call returns at once.
	var v vm.VM
	s.call(api.ToolCreateVM, map[string]any{
		"name": "node-1", "network": testNetwork, "wait_for": "none",
		"user_data": `{"ignition":{"version":"3.4.0"}}`, "metadata": map[string]any{"role": "control-plane"},
		"ssh_authorized_keys": []string{"ssh-ed25519 AAAA user@laptop"},
	}, &v)
	assert.NotEmpty(t, v.ID)
	assert.Equal(t, vm.StateInstalling, v.State)
	assert.Equal(t, testImageRef, v.Image, "the catalog default is pinned")
	assert.Equal(t, "1.32.0", v.KubernetesVersion, "the newest sysext version is picked")
	assert.Equal(t, api.DefaultCPUs, v.CPUs)
	assert.Equal(t, api.DefaultMemoryMiB, v.MemoryMiB)
	assert.Equal(t, api.DefaultDiskGiB, v.DiskGiB)
	assert.Equal(t, "node-1", v.Hostname)
	assert.True(t, v.RequireAttestation)
	assert.Equal(t, "10.10.0.2", v.IP)
	assert.Equal(t, map[string]string{"role": "control-plane"}, v.Metadata)

	var got, restGot vm.VM
	s.call(api.ToolGetVM, map[string]any{"id": v.ID}, &got)
	assert.Equal(t, v.ID, got.ID)
	require.Equal(t, http.StatusOK, getJSON(t, ts.URL+api.Prefix+"/vms/"+v.ID, &restGot))
	assert.Equal(t, got.ID, restGot.ID)
	assert.Equal(t, got.State, restGot.State)
	var vms []vm.VM
	s.call(api.ToolListVMs, nil, &vms)
	require.Len(t, vms, 1)

	var console api.ConsoleResponse
	s.call(api.ToolGetVMConsole, map[string]any{"id": v.ID, "lines": 5}, &console)
	assert.Equal(t, api.ConsoleResponse{ID: v.ID, Lines: 5}, console, "no console output yet")
	var metrics api.MetricsResponse
	s.call(api.ToolGetVMMetrics, map[string]any{"id": v.ID}, &metrics)
	assert.Equal(t, api.MetricsNote, metrics.Note)
	assert.Equal(t, json.RawMessage("null"), metrics.Report)
	var att vm.Attestation
	s.call(api.ToolGetVMAttestation, map[string]any{"id": v.ID}, &att)
	assert.True(t, att.Required)
	assert.False(t, att.UserDataReleased)

	s.call(api.ToolListNetworks, nil, &nets)
	require.Len(t, nets[0].Leases, 1, "the VM holds a lease")
	assert.Regexp(t, `^conflict: `, s.errText(api.ToolDeleteNetwork, map[string]any{"name": testNetwork}), "network in use")
	assert.Regexp(t, `^conflict: `, s.errText(api.ToolStartVM, map[string]any{"id": v.ID}), "already running")
	assert.Regexp(t, `^invalid_request: `, s.errText(api.ToolCreateVM, map[string]any{"name": "Bad Name", "network": testNetwork, "wait_for": "none"}))
	assert.Regexp(t, `^invalid_request: `, s.errText(api.ToolCreateVM, map[string]any{"name": "node-2", "network": "nope", "wait_for": "none"}), "an unknown network is a spec error")
	assert.Regexp(t, `^not_found: `, s.errText(api.ToolGetVM, map[string]any{"id": "nope"}))

	var deleted api.DeletedResponse
	s.call(api.ToolDeleteVM, map[string]any{"id": v.ID}, &deleted)
	assert.Equal(t, api.DeletedResponse{ID: v.ID, Deleted: true}, deleted)
	assert.Equal(t, http.StatusNotFound, getJSON(t, ts.URL+api.Prefix+"/vms/"+v.ID, nil))
	s.call(api.ToolDeleteNetwork, map[string]any{"name": testNetwork}, &deleted)
	assert.Equal(t, api.DeletedResponse{ID: testNetwork, Deleted: true}, deleted)
	s.call(api.ToolListNetworks, nil, &nets)
	assert.Empty(t, nets)

	var call mcp.CallToolRequest
	call.Params.Name = "no_such_tool"
	_, err = s.c.CallTool(ctx, call)
	require.Error(t, err, "unknown tools are a protocol error, not a result")
}
