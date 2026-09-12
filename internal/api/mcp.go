package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/host"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/metrics"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// MCP tool names (docs/design.md "MCP tool surface"). Through muster they
// appear as x_<server>_<tool>, e.g. x_vm-manager_get_host.
const (
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
		ToolGetHost, ToolListImages, ToolGetImage, ToolListNetworks, ToolGetNetwork,
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
}

const instructions = `Provision cloud-provider-like virtual machines on this KVM host: an instance metadata service (IMDS), a vTPM with measured boot, an immutable mkosi-built OS and Kubernetes as a sysext layer, so the VMs can be handed to the CAPI based cluster-manager. Call get_host first: it reports whether the host can run VMs (ready) and, if not, which prerequisites are missing — nothing else can succeed until they are met.

Lifecycle: list_images shows the bootable images and get_image the Kubernetes versions one offers. A VM needs a network: call create_network, or use the network named "default" that the server creates at startup (see list_networks), then create_vm. create_vm installs the image onto a fresh disk (state installing), reboots into it (booting), attests from the initrd when require_attestation is set (attesting) and is ready once the guest's service manager sent READY=1; running means the process is up but READY never arrived within the boot timeout. wait_for chooses how long create_vm blocks: none returns at once and you poll get_vm; installed, attested or ready block until that milestone (a timeout error still leaves the VM running). user_data is CAPI's bootstrap Secret as Ignition JSON; the IMDS releases it to the guest only after a verified attestation quote from the initrd (right after the installed boot starts when require_attestation is false), so a VM created without user_data boots the bare OS. get_vm reports state, IP, attestation and the last error; get_vm_console shows the serial console when something goes wrong; exec_vm runs a command as root over ssh through the virtual network; forward_port exposes a guest port such as 6443 on a loopback address of the host. stop_vm, start_vm and reboot_vm move a VM between stopped and running; delete_vm removes the VM with its disk, and delete_network succeeds only once no VM is attached.`

// hints are the four MCP tool annotations.
type hints struct{ readOnly, destructive, idempotent, openWorld bool }

var (
	hintRead        = hints{readOnly: true, idempotent: true}
	hintCreate      = hints{}
	hintIdempotent  = hints{idempotent: true}
	hintDestructive = hints{destructive: true, idempotent: true}
	hintExec        = hints{destructive: true, openWorld: true}
)

// newTool declares a tool with its description, all four annotations and
// its arguments.
func newTool(name, desc string, h hints, args ...mcp.ToolOption) mcp.Tool {
	opts := append([]mcp.ToolOption{
		mcp.WithDescription(desc),
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

// NewMCPServer builds an MCP server exposing the same operations as the REST
// API as tools. Results are JSON text with the same shapes as the REST bodies.
func NewMCPServer(svc Services, version string) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("vm-manager", version,
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions(instructions),
	)
	t := &tools{svc: svc}

	s.AddTool(newTool(ToolGetHost,
		"Read-only. Report the KVM host's capabilities: hostname, kernel, CPUs and memory; whether /dev/kvm and /dev/vhost-vsock are accessible; the qemu-system-x86_64, swtpm and systemd versions; the OVMF firmware image found; the systemd storage providers present; and ready plus the list of missing prerequisites. Call it before creating VMs.",
		hintRead), t.getHost)
	s.AddTool(newTool(ToolListImages,
		"Read-only. List the bootable images in the catalog (id, version, UKI and disk paths, the Kubernetes versions available as a sysext, the PCR policy). Empty means no image was built or --image-dir points elsewhere; create_vm needs at least one.",
		hintRead), t.listImages)
	s.AddTool(newTool(ToolGetImage,
		"Read-only. Describe one image. Call list_images first for the references.",
		hintRead,
		mcp.WithString(argRef, mcp.Required(), mcp.Description(`Image reference: "<id>_<version>" or the bare id for its newest version`)),
	), t.getImage)
	s.AddTool(newTool(ToolListNetworks,
		"Read-only. List the virtual networks with their gateway and the leases (VM id, MAC, IP) of attached VMs. The server creates a network named default at startup.",
		hintRead), t.listNetworks)
	s.AddTool(newTool(ToolGetNetwork,
		"Read-only. Describe one network: spec, gateway and leases. Call list_networks first for the names.",
		hintRead, nameArg), t.getNetwork)
	s.AddTool(newTool(ToolListVMs,
		"Read-only. List every VM record: id, name, state, IP, image, sizing, attestation and timestamps, oldest first.",
		hintRead), t.listVMs)
	s.AddTool(newTool(ToolGetVM,
		"Read-only. Fetch one VM record: state (creating, installing, booting, attesting, ready, running, stopping, stopped, failed, deleting), IP and MAC, attestation summary, the guest's last STATUS= text, lastError when something went wrong. Poll it after create_vm with wait_for none, or after start_vm and reboot_vm.",
		hintRead, idArg), t.vmOp(func(_ context.Context, id string) (*vm.VM, error) { return svc.VM.Get(id) }))
	s.AddTool(newTool(ToolGetVMConsole,
		"Read-only. Return the last lines of the VM's serial console; the first place to look when a VM is failed or stuck in installing or booting.",
		hintRead, idArg,
		mcp.WithNumber(argLines, mcp.Description("Number of trailing lines, default 100, at most 10000")),
	), t.getVMConsole)
	s.AddTool(newTool(ToolGetVMMetrics,
		"Read-only. Per-VM metrics. host: state, attestation, the QEMU process's cpu_seconds and memory_rss_bytes (absent while no process runs), disk_bytes, install_seconds, boot_to_ready_seconds and the byte counters of the VM's network. guest: a summary of the guest's last systemd-report upload (age, family and series counts, the first entries as sample), null until the guest sent one. Every series is on the Prometheus /metrics endpoint; the full upload is REST-only at raw_report_url. Call get_vm for state and IP.",
		hintRead, idArg), t.getVMMetrics)
	s.AddTool(newTool(ToolGetVMAttestation,
		"Read-only. Return the current boot's attestation: whether it is required, whether user-data was released, when the guest first asked for a nonce, and the verified/failed verdict of the initrd quote (gates user-data) and the ready quote (PCR 13 included). Reset on every installed boot.",
		hintRead, idArg), t.getVMAttestation)

	s.AddTool(newTool(ToolCreateNetwork,
		"WRITES: Create a virtual network for VMs (rootless, gvisor-tap-vsock) with DHCP, DNS and the instance metadata service reachable at 169.254.169.254 from every guest. Call it before create_vm unless the default network is enough. Returns the network with its gateway.",
		hintCreate,
		mcp.WithString(argName, mcp.Required(), mcp.Description("Network name, a DNS label of at most 63 characters")),
		mcp.WithString(argCIDR, mcp.Required(), mcp.Description("IPv4 subnet, /16 to /29, outside 169.254.0.0/16, e.g. 192.168.128.0/24; the gateway is its first address")),
		mcp.WithString(argDNSSearchDomain, mcp.Description("DNS search domain handed to guests over DHCP")),
	), t.createNetwork)
	s.AddTool(newTool(ToolDeleteNetwork,
		"WRITES (destructive): Tear a network down. Fails with conflict while any VM, running or stopped, is attached: delete_vm those first (list_networks shows the leases).",
		hintDestructive, nameArg), t.deleteNetwork)
	s.AddTool(newTool(ToolCreateVM,
		"WRITES: Create a VM: allocate a disk on the storage provider, a vTPM, a lease on the network and a vsock id; boot the installer, which copies the image onto the disk; reboot into it, attest, and serve user_data once attestation passed. Call get_host, list_images and create_network (or list_networks) first. With wait_for ready (the default) the call blocks until the guest is up, up to the install and boot timeouts; with none it returns at once and get_vm follows the progress. A failure returns the VM id in the error: get_vm_console shows why, delete_vm cleans up.",
		hintCreate,
		mcp.WithString(argName, mcp.Required(), mcp.Description("VM name, a DNS label of at most 63 characters; also the default hostname")),
		mcp.WithString(argImage, mcp.Description(`Image reference ("<id>_<version>" or bare id); default: the catalog's newest image`)),
		mcp.WithString(argKubernetesVersion, mcp.Description("Kubernetes sysext version served as /kubernetes-version; default: the newest the image offers. See get_image")),
		mcp.WithNumber(argCPUs, mcp.Description("Virtual CPUs, default 2")),
		mcp.WithNumber(argMemoryMiB, mcp.Description("Memory in MiB, default 2048, at least 256")),
		mcp.WithNumber(argDiskGiB, mcp.Description("Disk size in GiB, default 20")),
		mcp.WithString(argNetwork, mcp.Description(`Network to attach to, default "default"`)),
		mcp.WithString(argUserData, mcp.Description("CAPI bootstrap data as Ignition JSON, served at /user-data by the IMDS only after attestation; omit to boot the bare OS")),
		mcp.WithArray(argSSHAuthorizedKeys, mcp.Description("Public keys appended to root's authorized keys, after vm-manager's own per-VM key"), mcp.WithStringItems()),
		mcp.WithString(argHostname, mcp.Description("Guest hostname, default: the name")),
		mcp.WithObject(argMetadata, mcp.Description("String key/value pairs served as /metadata/<key>"), mcp.AdditionalProperties(map[string]any{"type": "string"})),
		mcp.WithBoolean(argRequireAttestation, mcp.Description("Release user-data only after a verified initrd quote, default true; false releases it as soon as the installed boot starts")),
		mcp.WithString(argWaitFor, mcp.Description("Milestone to block on: none, installed, attested or ready (default)"), mcp.Enum(string(vm.WaitNone), string(vm.WaitInstalled), string(vm.WaitAttested), string(vm.WaitReady))),
	), t.createVM)
	s.AddTool(newTool(ToolStartVM,
		"WRITES: Boot a stopped or failed VM from its installed disk; attestation runs again and user-data is gated again. Returns the record in state booting; poll get_vm for ready. Fails with conflict when the VM is running or never finished installing.",
		hintIdempotent, idArg), t.vmOp(func(ctx context.Context, id string) (*vm.VM, error) { return svc.VM.Start(ctx, id) }))
	s.AddTool(newTool(ToolStopVM,
		"WRITES: Shut a running VM down: ACPI power-down, then SIGTERM and SIGKILL after the stop timeout. Returns the record in state stopping; get_vm shows stopped once the process exited. The disk is kept; start_vm boots it again.",
		hintIdempotent, idArg), t.vmOp(func(ctx context.Context, id string) (*vm.VM, error) { return svc.VM.Stop(ctx, id) }))
	s.AddTool(newTool(ToolRebootVM,
		"WRITES: stop_vm followed by start_vm. Returns the record in state booting; poll get_vm.",
		hintIdempotent, idArg), t.vmOp(func(ctx context.Context, id string) (*vm.VM, error) { return svc.VM.Reboot(ctx, id) }))
	s.AddTool(newTool(ToolDeleteVM,
		"WRITES (destructive): Stop the VM if it runs, then remove its disk, vTPM state, lease, port forwards and record. Not undoable. Call it for failed VMs too; afterwards delete_network can succeed.",
		hintDestructive, idArg), t.deleteVM)
	s.AddTool(newTool(ToolExecVM,
		"WRITES (destructive): Run a command as root inside the guest over ssh through the virtual network, authenticated with vm-manager's per-VM key; the guest's host key is pinned on first use. Needs a VM that is ready or running with sshd up. A non-zero exit is a result (exitCode), not an error. Anything the command does happens for real.",
		hintExec, idArg,
		mcp.WithArray(argCommand, mcp.Required(), mcp.Description(`Program and arguments, e.g. ["systemctl", "is-system-running"]`), mcp.WithStringItems()),
	), t.execVM)
	s.AddTool(newTool(ToolForwardPort,
		"WRITES: Expose one guest TCP port on a loopback address of the host (e.g. 6443 for the API server, 22 for ssh) and return that address. The forward lives until the VM is deleted; the VM needs an IP, which it has once created.",
		hintIdempotent, idArg,
		mcp.WithNumber(argPort, mcp.Required(), mcp.Description("Guest TCP port, 1-65535")),
	), t.forwardPort)

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
