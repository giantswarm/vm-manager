package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/vm-manager/internal/host"
)

// MCP tool names. Through muster they appear as x_<server>_<tool>, e.g.
// x_vm-manager_get_host.
const (
	ToolGetHost = "get_host"
)

// ToolNames lists every tool the MCP server registers.
func ToolNames() []string {
	return []string{ToolGetHost}
}

// Services are the domain services both API surfaces expose. A new domain
// package plugs in here: add its Service, then register its tools in
// NewMCPServer and its routes in REST.Register, both over the same methods.
type Services struct {
	Host *host.Service
}

// NewMCPServer builds an MCP server exposing the same operations as the REST
// API as tools. Results are JSON text with the same shapes as the REST bodies.
func NewMCPServer(svc Services, version string) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("vm-manager", version,
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions("Provision cloud-provider-like virtual machines on this KVM host: an instance metadata service, a vTPM with measured boot, an immutable mkosi-built OS and Kubernetes as a sysext layer, so the VMs can be handed to the CAPI based cluster-manager. Call get_host first: it reports whether the host can run VMs (ready) and, if not, which prerequisites are missing — nothing else can succeed until they are met."),
	)
	t := &tools{svc: svc}

	s.AddTool(mcp.NewTool(ToolGetHost,
		mcp.WithDescription("Read-only. Report the KVM host's capabilities: hostname, kernel, CPUs and memory; whether /dev/kvm and /dev/vhost-vsock are accessible; the qemu-system-x86_64, swtpm and systemd versions; the OVMF firmware image found; the systemd storage providers present; and ready plus the list of missing prerequisites. Call it before creating VMs."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	), t.getHost)

	return s
}

// tools holds the MCP handlers; each is a thin adapter over a service method.
type tools struct {
	svc Services
}

func (t *tools) getHost(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info, err := t.svc.Host.Get(ctx)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(info)
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// errResult reports a failed operation as a tool result (never a Go error, so
// the model sees the message) with the same code the REST API would answer.
func errResult(err error) *mcp.CallToolResult {
	_, code := statusFor(err)
	return mcp.NewToolResultError(fmt.Sprintf("%s: %s", code, err.Error()))
}
