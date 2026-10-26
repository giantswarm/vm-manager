package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/buildinfo"
	"github.com/giantswarm/vm-manager/internal/host"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/metrics"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// MCP tool names (docs/design.md "MCP tool surface"). Through muster they
// appear as x_<server>_<tool>, e.g. x_vm-manager_get_host.
const (
	ToolGetInfo          = "get_info"
	ToolGetHost          = "get_host"
	ToolListImages       = "list_images"
	ToolGetImage         = "get_image"
	ToolListNetworks     = "list_networks"
	ToolGetNetwork       = "get_network"
	ToolListVMs          = "list_vms"
	ToolGetVM            = "get_vm"
	ToolGetVMConsole     = "get_vm_console"
	ToolGetVMMetrics     = "get_vm_metrics"
	ToolGetVMAttestation = "get_vm_attestation"
	ToolCreateNetwork    = "create_network"
	ToolDeleteNetwork    = "delete_network"
	ToolCreateVM         = "create_vm"
	ToolStartVM          = "start_vm"
	ToolStopVM           = "stop_vm"
	ToolRebootVM         = "reboot_vm"
	ToolDeleteVM         = "delete_vm"
	ToolExecVM           = "exec_vm"
	ToolForwardPort      = "forward_port"
)

// Tool argument names; the create bodies in vm.go use the same names.
const (
	argID                 = "id"
	argRef                = "ref"
	argName               = "name"
	argCIDR               = "cidr"
	argDNSSearchDomain    = "dns_search_domain"
	argLines              = "lines"
	argImage              = "image"
	argKubernetesVersion  = "kubernetes_version"
	argCPUs               = "cpus"
	argMemoryMiB          = "memory_mib"
	argDiskGiB            = "disk_gib"
	argNetwork            = "network"
	argUserData           = "user_data"
	argSSHAuthorizedKeys  = "ssh_authorized_keys"
	argHostname           = "hostname"
	argMetadata           = "metadata"
	argRequireAttestation = "require_attestation"
	argWaitFor            = "wait_for"
	argCommand            = "command"
	argPort               = "port"
)

// ToolNames lists every tool the MCP server registers, in registration order:
// the read-only tools first, then the writes.
func ToolNames() []string {
	return []string{
		ToolGetInfo, ToolGetHost, ToolListImages, ToolGetImage, ToolListNetworks, ToolGetNetwork,
		ToolListVMs, ToolGetVM, ToolGetVMConsole, ToolGetVMMetrics, ToolGetVMAttestation,
		ToolCreateNetwork, ToolDeleteNetwork, ToolCreateVM, ToolStartVM, ToolStopVM,
		ToolRebootVM, ToolDeleteVM, ToolExecVM, ToolForwardPort,
	}
}

// Services are the domain services both API surfaces expose. A new domain
// package plugs in here: add its Service, then register its tools in
// NewMCPServer and its routes in REST.Register, both over the same methods.
type Services struct {
	Host   *host.Service
	VM     *vm.Service
	Images *images.Catalog
	// Metrics summarizes a VM for get_vm_metrics; it must be the registry
	// the VM service reports to.
	Metrics *metrics.Registry
	// DefaultNetwork is the network create_vm attaches to when the request
	// names none, and the one the tool descriptions point at; empty means
	// the DefaultNetwork constant.
	DefaultNetwork string
}

func (s Services) defaultNetwork() string {
	if s.DefaultNetwork == "" {
		return DefaultNetwork
	}
	return s.DefaultNetwork
}

// instructionsFormat takes the default network's name.
const instructionsFormat = `Provision cloud-provider-like virtual machines on this KVM host: an instance metadata service (IMDS), a vTPM with measured boot, an immutable mkosi-built OS and Kubernetes as a sysext layer, so the VMs can be handed to the CAPI based cluster-manager. Call get_host first: it reports whether the host can run VMs (ready) and, if not, which prerequisites are missing — nothing else can succeed until they are met.

Lifecycle: list_images shows the bootable images and get_image the Kubernetes versions one offers. A VM needs a network: call create_network, or use the network named %q that the server creates at startup (see list_networks), then create_vm. create_vm installs the image onto a fresh disk (state installing), reboots into it (booting), attests from the initrd when require_attestation is set (attesting) and is ready once the guest's service manager sent READY=1; running means the process is up but READY never arrived within the boot timeout. wait_for chooses how long create_vm blocks: none returns at once and you poll get_vm; installed, attested or ready block until that milestone (a timeout error still leaves the VM running). user_data is CAPI's bootstrap Secret as Ignition JSON; the IMDS releases it to the guest only after a verified attestation quote from the initrd (right after the installed boot starts when require_attestation is false), so a VM created without user_data boots the bare OS. get_vm reports state, IP, attestation and the last error; get_vm_console shows the serial console when something goes wrong; exec_vm runs a command as root over ssh through the virtual network; forward_port exposes a guest port such as 6443 on a loopback address of the host. stop_vm, start_vm and reboot_vm move a VM between stopped and running; delete_vm removes the VM with its disk, and delete_network succeeds only once no VM is attached.`

// hints are the four MCP tool annotations.
type hints struct{ readOnly, destructive, idempotent, openWorld bool }

var (
	hintRead        = hints{readOnly: true, idempotent: true}
	hintWrite       = hints{}
	hintIdempotent  = hints{idempotent: true}
	hintDestructive = hints{destructive: true, idempotent: true}
	hintExec        = hints{destructive: true, openWorld: true}
)

// newTool declares a tool with its description, all four annotations and
// its arguments. The summary is the line muster's tool discovery shows; see
// mcp-toolkit's docs/conventions.md.
func newTool(name, summary, details string, h hints, args ...mcp.ToolOption) mcp.Tool {
	opts := append([]mcp.ToolOption{
		tooldesc.Describe(summary, details),
		mcp.WithReadOnlyHintAnnotation(h.readOnly),
		mcp.WithDestructiveHintAnnotation(h.destructive),
		mcp.WithIdempotentHintAnnotation(h.idempotent),
		mcp.WithOpenWorldHintAnnotation(h.openWorld),
	}, args...)
	return mcp.NewTool(name, opts...)
}

var (
	idArg   = mcp.WithString(argID, mcp.Required(), mcp.Description("VM id as returned by create_vm or list_vms"))
	nameArg = mcp.WithString(argName, mcp.Required(), mcp.Description("Network name as returned by create_network or list_networks"))
)

// Info is get_info's answer: the build this server runs and the tools it
// registers.
type Info struct {
	// Version is the release (the image tag), dev for an untagged local
	// build; Commit and Built identify the source and the build time.
	Version string   `json:"version"`
	Commit  string   `json:"commit"`
	Built   string   `json:"built"`
	Tools   []string `json:"tools"`
}

// NewMCPServer builds an MCP server exposing the same operations as the REST
// API as tools. Results are JSON text with the same shapes as the REST bodies.
// build is what get_info and the MCP server identity report as the version.
func NewMCPServer(svc Services, build buildinfo.Info) *mcpserver.MCPServer {
	opts := append(tracingOptions(),
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions(fmt.Sprintf(instructionsFormat, svc.defaultNetwork())),
	)
	s := mcpserver.NewMCPServer("vm-manager", build.Version, opts...)
	t := &tools{svc: svc, build: build}
	defaultNetwork := svc.defaultNetwork()

	s.AddTool(newTool(ToolGetInfo,
		"Report this server's build (version, commit, build time) and the names of its tools.", "",
		hintRead), t.getInfo)
	s.AddTool(newTool(ToolGetHost,
		"Report whether this KVM host can run VMs (ready, missing prerequisites) and its capabilities. Call it before create_vm.",
		"Also the hostname, kernel, CPUs and memory; access to /dev/kvm and /dev/vhost-vsock; the qemu, swtpm and systemd versions; "+
			"the storage providers; and the OVMF firmware VMs boot with, whose build golden PCR values must have been recorded with.",
		hintRead), t.getHost)
	s.AddTool(newTool(ToolListImages,
		"List the bootable VM images in the catalog and the Kubernetes versions each offers. create_vm needs at least one.",
		"Empty means no image was built or --image-dir points elsewhere.",
		hintRead), t.listImages)
	s.AddTool(newTool(ToolGetImage,
		"Describe one VM image: its artifacts, the Kubernetes versions it offers as a sysext and its PCR policy.",
		"list_images has the references.",
		hintRead,
		mcp.WithString(argRef, mcp.Required(), mcp.Description(`Image reference: "<id>_<version>" or the bare id for its newest version`)),
	), t.getImage)
	s.AddTool(newTool(ToolListNetworks,
		"List the virtual networks with their gateway and the leases (VM id, MAC, IP) of attached VMs.",
		fmt.Sprintf("The server creates a network named %s at startup.", defaultNetwork),
		hintRead), t.listNetworks)
	s.AddTool(newTool(ToolGetNetwork,
		"Describe one virtual network: spec, gateway and leases.",
		"list_networks has the names.",
		hintRead, nameArg), t.getNetwork)
	s.AddTool(newTool(ToolListVMs,
		"List every virtual machine (VM), oldest first: id, name, state, IP, image, sizing, attestation and timestamps.", "",
		hintRead), t.listVMs)
	s.AddTool(newTool(ToolGetVM,
		"Fetch one VM: state, IP and MAC, attestation summary, the guest's last STATUS= text, and lastError on failure.",
		"States: creating, installing, booting, attesting, ready, running, stopping, stopped, failed, deleting. "+
			"Poll it after create_vm with wait_for none, or after start_vm and reboot_vm.",
		hintRead, idArg), t.vmOp(func(_ context.Context, id string) (*vm.VM, error) { return svc.VM.Get(id) }))
	s.AddTool(newTool(ToolGetVMConsole,
		"Return a VM's serial console tail (boot log), the first place to look when a VM failed or hangs installing or booting.", "",
		hintRead, idArg,
		mcp.WithNumber(argLines, mcp.Description(fmt.Sprintf("Number of trailing lines, default %d, at most %d", DefaultConsoleLines, vm.MaxConsoleLines))),
	), t.getVMConsole)
	s.AddTool(newTool(ToolGetVMMetrics,
		"Report one VM's metrics: its QEMU process, disk, install and boot times, network traffic, and the guest's last report.",
		"host has the process's cpu_seconds and memory_rss_bytes (absent while no process runs), disk_bytes, install_seconds, "+
			"boot_to_ready_seconds and the network's byte counters. guest summarizes the guest's last systemd-report upload, null until one arrived. "+
			"Every series is also on the Prometheus /metrics endpoint; the full upload is REST-only at raw_report_url.",
		hintRead, idArg), t.getVMMetrics)
	s.AddTool(newTool(ToolGetVMAttestation,
		"Return the attestation of a VM's current boot: whether required, whether user-data was released, and the quote verdicts.",
		"The initrd quote gates user-data; the ready quote includes PCR 13. Reset on every installed boot.",
		hintRead, idArg), t.getVMAttestation)

	s.AddTool(newTool(ToolCreateNetwork,
		"Create a virtual network for VMs with DHCP, DNS and the instance metadata service at 169.254.169.254.",
		fmt.Sprintf("Rootless (gvisor-tap-vsock). Needed before create_vm unless the %s network is enough.", defaultNetwork),
		hintWrite,
		mcp.WithString(argName, mcp.Required(), mcp.Description(fmt.Sprintf("Network name, a DNS label of at most %d characters", vm.MaxNameLen))),
		mcp.WithString(argCIDR, mcp.Required(), mcp.Description("IPv4 subnet, /16 to /29, outside 169.254.0.0/16, e.g. 192.168.128.0/24; the gateway is its first address")),
		mcp.WithString(argDNSSearchDomain, mcp.Description("DNS search domain handed to guests over DHCP")),
	), t.createNetwork)
	s.AddTool(newTool(ToolDeleteNetwork,
		"Delete a virtual network. Fails with conflict while any VM, running or stopped, is attached to it.",
		"delete_vm those first; list_networks shows the leases.",
		hintDestructive, nameArg), t.deleteNetwork)
	s.AddTool(newTool(ToolCreateVM,
		"Create a VM from a catalog image: install it on a fresh disk and boot it; user_data is served once attestation passed.",
		"It allocates a disk on the storage provider, a vTPM, a network lease and a vsock id. "+
			"Call get_host, list_images and create_network (or list_networks) first. "+
			"wait_for ready (the default) blocks until the guest is up, up to the install and boot timeouts; none returns at once and get_vm follows the progress. "+
			"A failure returns the VM id in the error: get_vm_console shows why, delete_vm cleans up.",
		hintWrite,
		mcp.WithString(argName, mcp.Required(), mcp.Description(fmt.Sprintf("VM name, a DNS label of at most %d characters; also the default hostname", vm.MaxNameLen))),
		mcp.WithString(argImage, mcp.Description(`Image reference ("<id>_<version>" or bare id); default: the catalog's newest image`)),
		mcp.WithString(argKubernetesVersion, mcp.Description("Kubernetes sysext version served as /kubernetes-version; default: the newest the image offers. See get_image")),
		mcp.WithNumber(argCPUs, mcp.Description(fmt.Sprintf("Virtual CPUs, default %d", DefaultCPUs))),
		mcp.WithNumber(argMemoryMiB, mcp.Description(fmt.Sprintf("Memory in MiB, default %d, at least %d", DefaultMemoryMiB, vm.MinMemoryMiB))),
		mcp.WithNumber(argDiskGiB, mcp.Description(fmt.Sprintf("Disk size in GiB, default %d", DefaultDiskGiB))),
		mcp.WithString(argNetwork, mcp.Description(fmt.Sprintf("Network to attach to, default %q", defaultNetwork))),
		mcp.WithString(argUserData, mcp.Description("CAPI bootstrap data as Ignition JSON, served at /user-data by the IMDS only after attestation; omit to boot the bare OS")),
		mcp.WithArray(argSSHAuthorizedKeys, mcp.Description("Public keys appended to root's authorized keys, after vm-manager's own per-VM key"), mcp.WithStringItems()),
		mcp.WithString(argHostname, mcp.Description("Guest hostname, default: the name")),
		mcp.WithObject(argMetadata, mcp.Description("String key/value pairs served as /metadata/<key>"), mcp.AdditionalProperties(map[string]any{"type": "string"})),
		mcp.WithBoolean(argRequireAttestation, mcp.Description("Release user-data only after a verified initrd quote, default true; false releases it as soon as the installed boot starts")),
		mcp.WithString(argWaitFor, mcp.Description("Milestone to block on: none, installed, attested or ready (default)"), mcp.Enum(string(vm.WaitNone), string(vm.WaitInstalled), string(vm.WaitAttested), string(vm.WaitReady))),
	), t.createVM)
	s.AddTool(newTool(ToolStartVM,
		"Boot a stopped or failed VM from its installed disk; attestation runs again and gates user-data again.",
		"Returns the record in state booting; poll get_vm for ready. Fails with conflict when the VM is running or never finished installing.",
		hintIdempotent, idArg), t.vmOp(func(ctx context.Context, id string) (*vm.VM, error) { return svc.VM.Start(ctx, id) }))
	s.AddTool(newTool(ToolStopVM,
		"Shut a running VM down: ACPI power-down, then SIGTERM and SIGKILL after --stop-timeout. Its disk is kept.",
		"Blocks until the process exited and returns the record in state stopped; start_vm boots it again.",
		hintIdempotent, idArg), t.vmOp(func(ctx context.Context, id string) (*vm.VM, error) { return svc.VM.Stop(ctx, id) }))
	s.AddTool(newTool(ToolRebootVM,
		"Reboot a VM: stop_vm followed by start_vm.",
		"Returns the record in state booting; poll get_vm.",
		hintWrite, idArg), t.vmOp(func(ctx context.Context, id string) (*vm.VM, error) { return svc.VM.Reboot(ctx, id) }))
	s.AddTool(newTool(ToolDeleteVM,
		"Delete a VM, stopping it if it runs: its disk, vTPM state, lease, port forwards and record. Not undoable.",
		"Call it for failed VMs too; afterwards delete_network can succeed.",
		hintDestructive, idArg), t.deleteVM)
	s.AddTool(newTool(ToolExecVM,
		"Run a command as root inside a VM over ssh through the virtual network. Anything the command does happens for real.",
		"Authenticated with vm-manager's per-VM key; the guest's host key is pinned on first use. "+
			"Needs a VM that is ready or running with sshd up. A non-zero exit is a result (exitCode), not an error.",
		hintExec, idArg,
		mcp.WithArray(argCommand, mcp.Required(), mcp.Description(`Program and arguments, e.g. ["systemctl", "is-system-running"]`), mcp.WithStringItems()),
	), t.execVM)
	s.AddTool(newTool(ToolForwardPort,
		"Expose one guest TCP port (e.g. 6443 for the API server, 22 for ssh) on a loopback address of the host.",
		"Returns that address. Each call opens a new forward, which lives until the VM is deleted; the VM needs an IP, which it has once created.",
		hintWrite, idArg,
		mcp.WithNumber(argPort, mcp.Required(), mcp.Description("Guest TCP port, 1-65535")),
	), t.forwardPort)

	return s
}

// tools holds the MCP handlers; each is a thin adapter over a service method.
type tools struct {
	svc   Services
	build buildinfo.Info
}

func (t *tools) getInfo(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(Info{Version: t.build.Version, Commit: t.build.Commit, Built: t.build.Date, Tools: ToolNames()})
}

func (t *tools) getHost(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info, err := t.svc.Host.Get(ctx)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(info)
}

func (t *tools) listImages(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(t.svc.Images.List())
}

func (t *tools) getImage(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ref, err := req.RequireString(argRef)
	if err != nil {
		return invalidResult(err), nil
	}
	img, err := t.svc.Images.Get(ref)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(img)
}

func (t *tools) listNetworks(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(t.svc.VM.ListNetworks())
}

func (t *tools) getNetwork(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argName)
	if err != nil {
		return invalidResult(err), nil
	}
	n, err := t.svc.VM.GetNetwork(name)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(n)
}

func (t *tools) createNetwork(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var body CreateNetworkRequest
	if err := req.BindArguments(&body); err != nil {
		return invalidResult(err), nil
	}
	n, err := t.svc.VM.CreateNetwork(ctx, body.spec())
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(n)
}

func (t *tools) deleteNetwork(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString(argName)
	if err != nil {
		return invalidResult(err), nil
	}
	res, err := t.svc.deleteNetwork(ctx, name)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) listVMs(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(t.svc.VM.List())
}

func (t *tools) getVMConsole(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString(argID)
	if err != nil {
		return invalidResult(err), nil
	}
	res, err := t.svc.console(id, req.GetInt(argLines, DefaultConsoleLines))
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) getVMMetrics(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString(argID)
	if err != nil {
		return invalidResult(err), nil
	}
	res, err := t.svc.metrics(id)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) getVMAttestation(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString(argID)
	if err != nil {
		return invalidResult(err), nil
	}
	att, err := t.svc.VM.Attestation(id)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(att)
}

func (t *tools) createVM(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var body CreateVMRequest
	if err := req.BindArguments(&body); err != nil {
		return invalidResult(err), nil
	}
	v, err := t.svc.createVM(ctx, body)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(v)
}

// vmOp adapts a lifecycle method taking the VM id.
func (t *tools) vmOp(op func(ctx context.Context, id string) (*vm.VM, error)) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString(argID)
		if err != nil {
			return invalidResult(err), nil
		}
		v, err := op(ctx, id)
		if err != nil {
			return errResult(err), nil
		}
		return jsonResult(v)
	}
}

func (t *tools) deleteVM(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString(argID)
	if err != nil {
		return invalidResult(err), nil
	}
	res, err := t.svc.deleteVM(ctx, id)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) execVM(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString(argID)
	if err != nil {
		return invalidResult(err), nil
	}
	var body ExecRequest
	if err := req.BindArguments(&body); err != nil {
		return invalidResult(err), nil
	}
	res, err := t.svc.VM.Exec(ctx, id, body.Command)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
}

func (t *tools) forwardPort(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString(argID)
	if err != nil {
		return invalidResult(err), nil
	}
	port, err := req.RequireInt(argPort)
	if err != nil {
		return invalidResult(err), nil
	}
	res, err := t.svc.forward(ctx, id, port)
	if err != nil {
		return errResult(err), nil
	}
	return jsonResult(res)
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

// invalidResult is errResult for a malformed argument set.
func invalidResult(err error) *mcp.CallToolResult {
	return errResult(fmt.Errorf("%w: %v", apierr.ErrInvalid, err))
}
