//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// The test's network and VM. The CIDR stays clear of the server's default
// network (192.168.127.0/24) and of the docs' lab examples.
const (
	apiNetwork  = "e2e"
	apiCIDR     = "192.168.140.0/24"
	apiVMName   = "imds-e2e"
	apiHostname = "imds-e2e"
)

// Ceilings. create_vm with wait_for ready spans the installer boot and the
// installed boot; the record's timestamps split it into the two budgets of
// docs/design.md, printed as api_install_seconds= and
// api_boot_to_ready_seconds=.
const (
	apiTestTimeout    = 10 * time.Minute
	serverStartWithin = 30 * time.Second
	createVMWithin    = 5 * time.Minute
	apiInstallCeiling = 120 * time.Second
	apiBootCeiling    = 60 * time.Second
	// execWithin covers the gap between READY=1 and sshd answering.
	execWithin = 45 * time.Second
	// reportWithin covers vm-report-upload.timer's OnBootSec=15s plus an
	// upload.
	reportWithin = 60 * time.Second
	// childrenGoneWithin is how long the qemu and swtpm children may take
	// to be reaped after delete_vm returned.
	childrenGoneWithin = 10 * time.Second
	stopTimeout        = 30 * time.Second
	serverExitWithin   = stopTimeout + 30*time.Second
	dialTimeout        = 10 * time.Second
	pollEvery          = time.Second
)

// Guest-side facts: systemd-imds lives outside PATH, the import unit writes
// what it fetched to the credential store and logs these lines (systemd 261,
// src/imds/imds-tool.c).
const (
	guestIMDS        = "/usr/lib/systemd/systemd-imds"
	guestCredstore   = "/run/credstore" // #nosec G101 -- a directory, not a credential
	importUnit       = "systemd-imds-import.service"
	importedHostname = "Imported hostname as credential 'firstboot.hostname'."
	importedSSHKey   = "Imported SSH key as credential 'ssh.authorized_keys.root'."
	reportMediaType  = "application/vnd.io.systemd.report"
	sshBanner        = "SSH-2.0"
	// Serial console markers of an installed boot: OVMF's boot manager
	// entry (systemd-boot, installed by bootctl during sysinstall) and the
	// unit name in "Reached target Multi-User System" (colour escapes sit
	// between the words).
	consoleBootManager = `"Linux Boot Manager"`
	consoleMultiUser   = "Multi-User System"
)

// TestNetworkIMDS drives a real vm-manager serve process through its MCP
// endpoint: create a network and a VM on it, prove that the guest reached the
// IMDS over the virtual network (hostname, ssh keys and instance id answered,
// the credential import ran, the metrics upload arrived), that the installed
// system is healthy (running, no failed unit), that the console and a port
// forward work, and that delete_vm and delete_network leave nothing behind,
// including after the server's own SIGTERM shutdown.
func TestNetworkIMDS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), apiTestTimeout)
	defer cancel()
	requireHost(t)
	image := locateImage(t)
	dir := stateDir(t)
	_, testKey := generateSSHKey(t, dir)

	srv := startServer(ctx, t, dir, image.Dir)
	m := newMCPClient(ctx, t, srv.URL)

	tools, err := m.c.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, api.ToolNames(), names, "list_tools")

	prefix := netip.MustParsePrefix(apiCIDR)
	var n vm.NetworkInfo
	m.call(ctx, api.ToolCreateNetwork, map[string]any{"name": apiNetwork, "cidr": apiCIDR}, &n)
	assert.True(t, prefix.Contains(netip.MustParseAddr(n.Gateway)), "gateway %s outside %s", n.Gateway, apiCIDR)

	started := time.Now()
	var v vm.VM
	m.call(ctx, api.ToolCreateVM, map[string]any{
		"name":                apiVMName,
		"hostname":            apiHostname,
		"network":             apiNetwork,
		"ssh_authorized_keys": []string{testKey},
		"require_attestation": false,
		"wait_for":            string(vm.WaitReady),
	}, &v)
	createSeconds := time.Since(started).Seconds()
	t.Logf("create_vm returned after %.1fs: id %s state %s ip %s", createSeconds, v.ID, v.State, v.IP)
	require.Equal(t, vm.StateReady, v.State, "create_vm with wait_for ready: lastError %q\n%s", v.LastError, srv.logTail())

	m.call(ctx, api.ToolGetVM, map[string]any{"id": v.ID}, &v)
	require.Equal(t, vm.StateReady, v.State, "get_vm: lastError %q", v.LastError)
	assert.True(t, prefix.Contains(netip.MustParseAddr(v.IP)), "ip %s outside %s", v.IP, apiCIDR)
	assert.Equal(t, apiHostname, v.Hostname)
	require.NotNil(t, v.InstalledAt, "installedAt")
	require.NotNil(t, v.BootedAt, "bootedAt")
	require.NotNil(t, v.ReadyAt, "readyAt")
	install := v.InstalledAt.Sub(v.CreatedAt)
	boot := v.ReadyAt.Sub(*v.BootedAt)
	assert.Less(t, install, apiInstallCeiling, "install phase")
	assert.Less(t, boot, apiBootCeiling, "installed boot to READY=1")

	// The server's children are the VM's qemu and swtpm; delete_vm and the
	// shutdown must leave none of them behind.
	children := childrenOf(t, srv.pid())
	t.Logf("children of vm-manager serve: %v", children)
	assert.GreaterOrEqual(t, len(children), 2, "expected qemu and swtpm as children of the server")

	g := &guest{m: m, id: v.ID}
	g.waitSSH(ctx)
	assertGuest(ctx, t, g, v, testKey)

	// The console is the installed boot's: the firmware picked the boot
	// manager entry bootctl installed (the installer phase is a -kernel
	// direct boot) and the boot reached multi-user before READY=1.
	var console api.ConsoleResponse
	m.call(ctx, api.ToolGetVMConsole, map[string]any{"id": v.ID, "lines": 10000}, &console)
	assert.Equal(t, v.ID, console.ID)
	for _, want := range []string{consoleBootManager, consoleMultiUser} {
		assert.Contains(t, console.Console, want, "serial console lacks %q; last lines:\n%s", want, tail(console.Console, 20))
	}

	assertReport(ctx, t, m, v.ID)

	var fwd api.ForwardResponse
	m.call(ctx, api.ToolForwardPort, map[string]any{"id": v.ID, "port": 22}, &fwd)
	assert.Equal(t, 22, fwd.Port)
	assertSSHBanner(t, fwd.Address)

	var deleted api.DeletedResponse
	m.call(ctx, api.ToolDeleteVM, map[string]any{"id": v.ID}, &deleted)
	assert.True(t, deleted.Deleted)
	var vms []vm.VM
	m.call(ctx, api.ToolListVMs, map[string]any{}, &vms)
	assert.Empty(t, vms, "list_vms after delete_vm")
	assertGone(t, children, childrenGoneWithin, "delete_vm")

	m.call(ctx, api.ToolDeleteNetwork, map[string]any{"name": apiNetwork}, &deleted)
	assert.True(t, deleted.Deleted)
	var networks []vm.NetworkInfo
	m.call(ctx, api.ToolListNetworks, map[string]any{}, &networks)
	for _, x := range networks {
		assert.NotEqual(t, apiNetwork, x.Spec.Name, "list_networks after delete_network")
	}

	srv.stop()
	assertGone(t, children, 0, "the server's shutdown")

	t.Logf("api_create_vm_seconds=%.1f api_install_seconds=%.1f api_boot_to_ready_seconds=%.1f", createSeconds, install.Seconds(), boot.Seconds())
	fmt.Printf("api_create_vm_seconds=%.1f\napi_install_seconds=%.1f\napi_boot_to_ready_seconds=%.1f\n", createSeconds, install.Seconds(), boot.Seconds())
}

// assertGuest checks the installed system over exec_vm. The hostname and the
// ssh keys also travel as SMBIOS credentials (internal/vm passes
// firstboot.hostname and ssh.authorized_keys.root), so the static hostname
// alone proves nothing about the IMDS; what does is systemd-imds answering
// over the virtual network, the import unit's journal and its credentials.
func assertGuest(ctx context.Context, t *testing.T, g *guest, v vm.VM, testKey string) {
	t.Helper()
	assert.Equal(t, apiHostname, g.sh(ctx, "hostnamectl --static"), "static hostname")

	assert.Equal(t, apiHostname, g.sh(ctx, guestIMDS+" -K hostname"), "IMDS /hostname")
	assert.Equal(t, v.SSHPublicKey, g.sh(ctx, guestIMDS+" -K ssh-key"), "IMDS /public-keys/0 is vm-manager's per-VM key")
	assert.Equal(t, testKey, g.sh(ctx, guestIMDS+" /public-keys/1"), "IMDS /public-keys/1 is the caller's key")
	assert.Equal(t, v.ID, g.sh(ctx, guestIMDS+" /instance-id"), "IMDS /instance-id")

	journal := g.sh(ctx, "journalctl -b -u "+importUnit+" -o cat --no-pager")
	assert.Contains(t, journal, importedHostname, "%s journal:\n%s", importUnit, journal)
	assert.Contains(t, journal, importedSSHKey, "%s journal:\n%s", importUnit, journal)
	assert.Equal(t, "active", g.sh(ctx, "systemctl show "+importUnit+" -p ActiveState --value"), "%s journal:\n%s", importUnit, journal)
	assert.Equal(t, "success", g.sh(ctx, "systemctl show "+importUnit+" -p Result --value"), "%s journal:\n%s", importUnit, journal)
	assert.Equal(t, apiHostname, g.sh(ctx, "cat "+guestCredstore+"/firstboot.hostname"), "credential imported from the IMDS")
	assert.Contains(t, g.sh(ctx, "cat "+guestCredstore+"/ssh.authorized_keys.root"), v.SSHPublicKey, "credential imported from the IMDS")

	authorized := g.sh(ctx, "cat /root/.ssh/authorized_keys")
	assert.Contains(t, authorized, v.SSHPublicKey, "root's authorized_keys lacks vm-manager's key")
	assert.Contains(t, authorized, testKey, "root's authorized_keys lacks the caller's key")

	failed := g.sh(ctx, "systemctl --failed --no-legend --plain")
	assert.Empty(t, failed, "failed units in the installed system:\n%s", failed)
	state, err := g.run(ctx, "systemctl is-system-running")
	require.NoError(t, err)
	assert.Equal(t, "running", strings.TrimSpace(state.Stdout), "system state (exit %d)", state.ExitCode)
}

// assertReport waits for the guest's first vm-report-upload.timer run to
// show up as get_vm_metrics' report: systemd-report posted to the IMDS.
func assertReport(ctx context.Context, t *testing.T, m *mcpClient, id string) {
	t.Helper()
	deadline := time.Now().Add(reportWithin)
	for {
		var metrics api.MetricsResponse
		m.call(ctx, api.ToolGetVMMetrics, map[string]any{"id": id}, &metrics)
		if len(metrics.Report) > 0 && string(metrics.Report) != "null" {
			var report struct {
				MediaType string            `json:"mediaType"`
				Metrics   []json.RawMessage `json:"metrics"`
			}
			require.NoError(t, json.Unmarshal(metrics.Report, &report), "report: %s", metrics.Report)
			assert.Equal(t, reportMediaType, report.MediaType)
			assert.NotEmpty(t, report.Metrics)
			t.Logf("systemd-report upload received: %d metrics", len(report.Metrics))
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no systemd-report upload within %s (%s)", reportWithin, metrics.Note)
		}
		time.Sleep(pollEvery)
	}
}

// assertSSHBanner connects to a forward_port address and expects sshd's
// version string.
func assertSSHBanner(t *testing.T, address string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, dialTimeout)
	require.NoError(t, err, "forward_port address %s", address)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(dialTimeout)))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	require.NoError(t, err, "reading the ssh banner through %s", address)
	assert.True(t, strings.HasPrefix(string(buf[:n]), sshBanner), "banner through %s: %q", address, buf[:n])
}

// assertGone requires every pid to have disappeared within the grace period.
func assertGone(t *testing.T, pids map[int]string, within time.Duration, after string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var left []string
		for pid, comm := range pids {
			if alive(pid) {
				left = append(left, fmt.Sprintf("%s (pid %d)", comm, pid))
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("processes left after %s: %s", after, strings.Join(left, ", "))
			return
		}
		time.Sleep(pollEvery)
	}
}

// server is a vm-manager serve child process built from the working tree.
type server struct {
	t   *testing.T
	cmd *exec.Cmd
	// URL is the server's base URL; REST under /api/v1, MCP under /mcp.
	URL string
	log string
	// exited is closed once Wait returned; waitErr is valid after that.
	exited  chan struct{}
	waitErr error
}

// startServer builds the binary, starts it on a free loopback port with a
// state dir under dir and waits for /readyz. The process is killed on
// cleanup if the test did not stop it; its log is quoted on failure.
func startServer(ctx context.Context, t *testing.T, dir, imageDir string) *server {
	t.Helper()
	bin := filepath.Join(dir, "vm-manager")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, ".") // #nosec G204 -- the go toolchain, output under the test's state dir
	build.Dir = repoRoot(t)
	out, err := build.CombinedOutput()
	require.NoError(t, err, "go build: %s", out)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
	logPath := filepath.Join(dir, "serve.log")
	logFile, err := os.Create(logPath) // #nosec G304 -- under the test's state dir
	require.NoError(t, err)
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(bin, "-v", "serve", // #nosec G204 -- the binary built above
		"--listen", addr,
		"--state-dir", filepath.Join(dir, "state"),
		"--image-dir", imageDir,
		"--stop-timeout", stopTimeout.String(),
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start())
	s := &server{t: t, cmd: cmd, URL: "http://" + addr, log: logPath, exited: make(chan struct{})}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.exited)
	}()
	t.Cleanup(s.cleanup)
	t.Logf("vm-manager serve pid %d on %s, log %s", cmd.Process.Pid, s.URL, logPath)
	s.waitReady()
	return s
}

func (s *server) pid() int { return s.cmd.Process.Pid }

// waitReady polls /readyz until it answers 200.
func (s *server) waitReady() {
	s.t.Helper()
	deadline := time.Now().Add(serverStartWithin)
	for {
		select {
		case <-s.exited:
			s.t.Fatalf("vm-manager serve exited before it was ready: %v\n%s", s.waitErr, s.logTail())
		default:
		}
		resp, err := http.Get(s.URL + "/readyz") // #nosec G107 -- loopback URL built above
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("no 200 from /readyz within %s (last: %v)\n%s", serverStartWithin, err, s.logTail())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// stop sends SIGTERM and requires a clean exit within the stop timeout plus
// the service's close grace.
func (s *server) stop() {
	s.t.Helper()
	require.NoError(s.t, s.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case <-s.exited:
		assert.NoError(s.t, s.waitErr, "vm-manager serve exit status after SIGTERM\n%s", s.logTail())
	case <-time.After(serverExitWithin):
		s.t.Fatalf("vm-manager serve still running %s after SIGTERM\n%s", serverExitWithin, s.logTail())
	}
}

// cleanup kills a server the test left running, then whatever children it
// had, so a failed test does not leak a VM.
func (s *server) cleanup() {
	if s.t.Failed() {
		s.t.Logf("%s", s.logTail())
	}
	select {
	case <-s.exited:
		return
	default:
	}
	children := childrenOf(s.t, s.pid())
	_ = s.cmd.Process.Kill()
	<-s.exited
	for pid := range children {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// logTail is the end of the server log, labelled, for failure messages.
func (s *server) logTail() string {
	b, err := os.ReadFile(s.log) // #nosec G304 -- the log file created above
	if err != nil {
		return fmt.Sprintf("(server log unreadable: %v)", err)
	}
	return fmt.Sprintf("--- last %d lines of %s ---\n%s\n--- end server log ---", consoleTailLines, s.log, tail(string(b), consoleTailLines))
}

// freePort asks the kernel for an unused loopback TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// childrenOf lists the direct children of pid by the PPid in their /proc
// status, with their command names for the log; nothing is matched by name.
func childrenOf(t *testing.T, pid int) map[int]string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	require.NoError(t, err)
	want := "\nPPid:\t" + strconv.Itoa(pid) + "\n"
	children := map[int]string{}
	for _, e := range entries {
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status")) // #nosec G304 -- /proc/<pid>/status
		if err != nil || !strings.Contains(string(status), want) {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "comm")) // #nosec G304 -- /proc/<pid>/comm
		children[child] = strings.TrimSpace(string(comm))
	}
	return children
}

// alive reports whether /proc still has an entry for pid (a zombie counts:
// it has not been reaped).
func alive(pid int) bool {
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	return err == nil
}

// mcpClient is an initialized streamable HTTP session with the server.
type mcpClient struct {
	t *testing.T
	c *mcpclient.Client
}

func newMCPClient(ctx context.Context, t *testing.T, url string) *mcpClient {
	t.Helper()
	c, err := mcpclient.NewStreamableHttpClient(url+"/mcp", transport.WithHTTPTimeout(createVMWithin+time.Minute))
	require.NoError(t, err)
	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() { _ = c.Close() })
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "vm-manager-e2e", Version: "test"}
	res, err := c.Initialize(ctx, init)
	require.NoError(t, err)
	t.Logf("mcp server %s %s, protocol %s", res.ServerInfo.Name, res.ServerInfo.Version, res.ProtocolVersion)
	return &mcpClient{t: t, c: c}
}

// callRaw invokes a tool; a failed operation comes back as a result with
// IsError set, never as a transport error.
func (m *mcpClient) callRaw(ctx context.Context, name string, args map[string]any) *mcp.CallToolResult {
	m.t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := m.c.CallTool(ctx, req)
	require.NoError(m.t, err, "%s transport", name)
	return res
}

// call invokes a tool that must succeed and decodes its JSON text into out.
func (m *mcpClient) call(ctx context.Context, name string, args map[string]any, out any) {
	m.t.Helper()
	res := m.callRaw(ctx, name, args)
	text := resultText(m.t, res)
	require.False(m.t, res.IsError, "%s(%v): %s", name, args, text)
	if out != nil {
		require.NoError(m.t, json.Unmarshal([]byte(text), out), "%s result: %s", name, text)
	}
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, res.Content, 1, "tool results carry one content item")
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "tool results are text, got %T", res.Content[0])
	return tc.Text
}

// guest runs commands in the VM through exec_vm.
type guest struct {
	m  *mcpClient
	id string
}

// run executes a shell command line as root. A guest whose sshd is not up yet
// makes exec_vm fail as a whole; that is returned as err, a non-zero exit is a
// result.
func (g *guest) run(ctx context.Context, command string) (vm.ExecResult, error) {
	g.m.t.Helper()
	res := g.m.callRaw(ctx, api.ToolExecVM, map[string]any{"id": g.id, "command": []string{"sh", "-c", command}})
	text := resultText(g.m.t, res)
	if res.IsError {
		return vm.ExecResult{}, errors.New(text)
	}
	var out vm.ExecResult
	require.NoError(g.m.t, json.Unmarshal([]byte(text), &out), "exec_vm result: %s", text)
	return out, nil
}

// sh runs command, requires exit 0 and returns the trimmed stdout.
func (g *guest) sh(ctx context.Context, command string) string {
	g.m.t.Helper()
	res, err := g.run(ctx, command)
	require.NoError(g.m.t, err, "exec_vm %q", command)
	require.Equal(g.m.t, 0, res.ExitCode, "exec_vm %q: stdout %q stderr %q", command, res.Stdout, res.Stderr)
	return strings.TrimSpace(res.Stdout)
}

// waitSSH repeats a trivial command until sshd answers: READY=1 precedes the
// ssh listener by a moment.
func (g *guest) waitSSH(ctx context.Context) {
	g.m.t.Helper()
	deadline := time.Now().Add(execWithin)
	for attempt := 1; ; attempt++ {
		res, err := g.run(ctx, "true")
		if err == nil && res.ExitCode == 0 {
			g.m.t.Logf("exec_vm answered on attempt %d", attempt)
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			g.m.t.Fatalf("exec_vm did not succeed within %s: %v %+v", execWithin, err, res)
		}
		time.Sleep(pollEvery)
	}
}
