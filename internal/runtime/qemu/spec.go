package qemu

import (
	"encoding/base64"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

// Phase is the boot flow a Spec describes.
type Phase string

const (
	// PhaseInstall is the installer boot: UKI via -kernel, installer DDI
	// plus blank target, systemd-sysinstall runs and reboots.
	PhaseInstall Phase = "install"
	// PhaseBoot is every boot from the installed target disk.
	PhaseBoot Phase = "boot"
)

// Disk serials the guest relies on: the installer unit finds the destination
// at /dev/disk/by-id/virtio-target and the base image at virtio-installer.
const (
	SerialInstaller = "installer"
	SerialTarget    = "target"
)

// SMBIOS type 1 defaults. The guest image's IMDS units start only when the
// DMI system vendor and product are these (hwdb glob dmi:*:svnGiantSwarm:*),
// so a Spec normally leaves SMBIOS zero.
const (
	DefaultManufacturer = "GiantSwarm"
	DefaultProduct      = "vm-manager"
)

// Credential names the guest image consumes (systemd.system-credentials(7)
// plus the image's own vm.install-target), so callers and tests spell them
// once. system.machine_id must be the same value in both phases: the
// installed system's var mount is keyed by it.
//
//nolint:gosec // G101: these are credential names, not secrets
const (
	// CredentialNotifySocket is where PID 1 sends READY=1;
	// NotifyListener.Credential renders the value.
	CredentialNotifySocket = "vmm.notify_socket"
	// CredentialInstallTarget names the disk systemd-sysinstall writes to;
	// InstallTargetDevice is its value for Spec.Target.
	CredentialInstallTarget = "vm.install-target"
	// CredentialMachineID seeds /etc/machine-id (32 hex characters).
	CredentialMachineID = "system.machine_id"
	// CredentialHostname is the static hostname systemd-firstboot sets.
	CredentialHostname = "firstboot.hostname"
	// CredentialSSHAuthorizedKeysRoot lands in /root/.ssh/authorized_keys.
	CredentialSSHAuthorizedKeysRoot = "ssh.authorized_keys.root"
)

// InstallTargetDevice is the guest path of the target disk, from its
// virtio-blk serial.
const InstallTargetDevice = "/dev/disk/by-id/virtio-" + SerialTarget

// Vsock CID bounds: 0-2 are reserved (any, local, host).
const (
	MinCID = 3
	MaxCID = 0xFFFFFFFE
)

// SMBIOS type 11 string prefixes systemd consumes: text and binary (base64)
// credentials, and the extra kernel command line for systemd-stub.
//
//nolint:gosec // G101: SMBIOS string prefixes, not secrets
const (
	credentialPrefix       = "io.systemd.credential:"
	credentialBinaryPrefix = "io.systemd.credential.binary:"
	cmdlineExtraPrefix     = "io.systemd.stub.kernel-cmdline-extra="
)

// virtioSerialMax is VIRTIO_BLK_ID_BYTES, the longest serial virtio-blk
// reports to the guest.
const virtioSerialMax = 20

// serialPattern is what a disk serial and a netdev id may contain: they end
// up in udev symlinks and QEMU ids.
var serialPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// MaxUnixSocketPath is the longest path a unix socket can be bound to
// (sizeof(sun_path) - 1 on Linux). Longer state dirs fail late inside QEMU or
// swtpm with an opaque bind error, so Spec.validate rejects them up front.
const MaxUnixSocketPath = 107

// driveFormats are the image formats a Drive may declare.
var driveFormats = map[string]bool{"raw": true, "qcow2": true}

// netdevBackendPattern requires the backend type as the first token; the
// remaining options are the caller's, escaped with EscapeOption.
var netdevBackendPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(,[A-Za-z0-9_.-]+=.*)?$`)

// Spec is everything Command needs to build the argument list of one VM.
type Spec struct {
	// ID names the VM: the SMBIOS type 1 serial (unless SMBIOS.Serial is
	// set) and the QEMU guest name seen in ps.
	ID string
	// Phase selects the boot flow.
	Phase Phase
	// CPUs and MemoryMiB size the machine.
	CPUs      int
	MemoryMiB int
	// UKI is the unified kernel image booted directly in PhaseInstall;
	// ignored in PhaseBoot.
	UKI string
	// Installer is the base DDI, attached read-only as serial=installer in
	// PhaseInstall; ignored in PhaseBoot.
	Installer string
	// Target is the persistent disk (serial=target): the installation
	// destination in PhaseInstall, the boot disk in PhaseBoot.
	Target string
	// ExtraDrives are attached after the target, in order.
	ExtraDrives []Drive
	// Netdevs are the network interfaces, in order; the network package
	// builds them.
	Netdevs []Netdev
	// SMBIOS is the type 1 (system) record; zero fields take the defaults.
	SMBIOS SMBIOS
	// Credentials are passed as SMBIOS type 11 io.systemd.credential strings
	// (the Credential* names: vmm.notify_socket, system.machine_id,
	// firstboot.*, ssh.authorized_keys.root, and vm.install-target in
	// PhaseInstall). Values that are not printable or span lines go base64
	// as io.systemd.credential.binary.
	Credentials map[string]string
	// KernelCmdlineExtra is appended to the UKI command line by systemd-stub
	// (io.systemd.stub.kernel-cmdline-extra); "ignition.firstboot" on the
	// first installed boot.
	KernelCmdlineExtra string
	// VsockCID is the guest's vsock context id, MinCID..MaxCID.
	VsockCID uint32
	// TPMSocket is the swtpm control socket (tpm.Instance.SocketPath);
	// empty runs the VM without a vTPM.
	TPMSocket string
	// OVMFCode is the read-only firmware image, OVMFVars the per-VM
	// writable copy of the variable store (Runtime creates it).
	OVMFCode string
	OVMFVars string
	// SerialLog receives the guest serial console (the first serial port,
	// ttyS0, which the image's kernel command line uses at 115200 baud).
	SerialLog string
	// QMPSocket is the unix socket QEMU serves QMP on.
	QMPSocket string
	// NoReboot makes a guest reboot end the process instead (PhaseInstall).
	NoReboot bool
}

// Drive is an additional virtio-blk disk.
type Drive struct {
	// Path is the image or block device.
	Path string
	// Serial identifies the disk to the guest (virtio-<Serial>) and is the
	// QEMU drive id; 1-20 characters of [A-Za-z0-9._-], unique per VM.
	Serial string
	// Format is the image format, "raw" (default when empty) or "qcow2".
	Format string
	// ReadOnly attaches the disk read-only.
	ReadOnly bool
}

// Netdev is one network interface: a QEMU netdev backend and the virtio-net
// device on it. The network package passes the gvisor-tap-vsock endpoint as
//
//	Netdev{ID: "net0", MAC: mac, Backend: "stream,addr.type=unix,addr.path=/run/vm-manager/net/<vm>.sock,reconnect-ms=1000"}
//
// QEMU 11.1 syntax (qemu-system-x86_64 -help):
//
//	-netdev stream,id=str[,server=on|off],addr.type=unix,addr.path=path[,abstract=on|off][,tight=on|off][,reconnect-ms=milliseconds]
//
// The reconnect knob is reconnect-ms; the older reconnect=seconds form was
// deprecated in QEMU 9.2 and is gone in 11. Command appends id=ID to the
// backend, so Backend must not carry its own id.
type Netdev struct {
	// ID is the netdev id shared by -netdev and the device; [A-Za-z0-9._-].
	ID string
	// Backend is the -netdev value without id, type first ("user",
	// "stream,addr.type=unix,addr.path=..."). Callers must run every embedded
	// value (paths in particular) through EscapeOption, because a bare comma
	// starts a new QEMU option.
	Backend string
	// MAC is the guest's Ethernet address, what the DHCP lease is keyed by.
	MAC string
}

// SMBIOS is the type 1 system record.
type SMBIOS struct {
	Manufacturer string
	Product      string
	Version      string
	// Serial defaults to Spec.ID.
	Serial string
}

// Command builds the qemu-system-x86_64 arguments for spec, in a fixed
// order, without the binary. It validates the spec and wraps every problem
// in apierr.ErrInvalid.
func Command(spec Spec) ([]string, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	install := spec.Phase == PhaseInstall
	args := []string{
		"-name", "guest=" + escape(spec.ID),
		"-machine", "q35",
		"-accel", "kvm",
		"-cpu", "host",
		"-smp", strconv.Itoa(spec.CPUs),
		"-m", strconv.Itoa(spec.MemoryMiB) + "M",
		"-nodefaults",
		"-display", "none",
		"-serial", "file:" + escape(spec.SerialLog),
		"-qmp", "unix:" + escape(spec.QMPSocket) + ",server=on,wait=off",
		"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=" + escape(spec.OVMFCode),
		"-drive", "if=pflash,format=raw,unit=1,file=" + escape(spec.OVMFVars),
	}
	if install {
		args = append(args, "-kernel", spec.UKI)
		args = append(args, driveArgs(Drive{Path: spec.Installer, Serial: SerialInstaller, ReadOnly: true}, -1)...)
	}
	bootindex := -1
	if !install {
		bootindex = 0
	}
	args = append(args, driveArgs(Drive{Path: spec.Target, Serial: SerialTarget}, bootindex)...)
	for _, d := range spec.ExtraDrives {
		args = append(args, driveArgs(d, -1)...)
	}
	for _, n := range spec.Netdevs {
		args = append(args,
			"-netdev", n.Backend+",id="+n.ID,
			"-device", "virtio-net-pci,netdev="+n.ID+",mac="+n.MAC,
		)
	}
	args = append(args, "-device", "vhost-vsock-pci,id=vsock0,guest-cid="+strconv.FormatUint(uint64(spec.VsockCID), 10))
	if spec.TPMSocket != "" {
		args = append(args,
			"-chardev", "socket,id=chrtpm,path="+escape(spec.TPMSocket),
			"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
			"-device", "tpm-crb,tpmdev=tpm0",
		)
	}
	args = append(args, "-smbios", spec.smbiosType1())
	for _, s := range renderCredentials(spec.Credentials) {
		args = append(args, "-smbios", "type=11,value="+escape(s))
	}
	if spec.KernelCmdlineExtra != "" {
		args = append(args, "-smbios", "type=11,value="+escape(cmdlineExtraPrefix+spec.KernelCmdlineExtra))
	}
	if spec.NoReboot {
		args = append(args, "-no-reboot")
	}
	return args, nil
}

// driveArgs is the -drive/-device pair for d; bootindex < 0 sets none. The
// aio backend is left at QEMU's default on purpose: hosts with
// io_uring_disabled=2 reject aio=io_uring, and the default (threads) works
// everywhere.
func driveArgs(d Drive, bootindex int) []string {
	format := d.Format
	if format == "" {
		format = "raw"
	}
	drive := "if=none,id=" + d.Serial + ",format=" + format + ",file=" + escape(d.Path)
	if d.ReadOnly {
		drive += ",readonly=on"
	} else {
		drive += ",discard=unmap"
	}
	dev := "virtio-blk-pci,drive=" + d.Serial + ",serial=" + d.Serial
	if bootindex >= 0 {
		dev += ",bootindex=" + strconv.Itoa(bootindex)
	}
	return []string{"-drive", drive, "-device", dev}
}

func (s Spec) smbiosType1() string {
	m := s.SMBIOS
	if m.Manufacturer == "" {
		m.Manufacturer = DefaultManufacturer
	}
	if m.Product == "" {
		m.Product = DefaultProduct
	}
	if m.Serial == "" {
		m.Serial = s.ID
	}
	out := "type=1,manufacturer=" + escape(m.Manufacturer) + ",product=" + escape(m.Product)
	if m.Version != "" {
		out += ",version=" + escape(m.Version)
	}
	return out + ",serial=" + escape(m.Serial)
}

// renderCredentials is the sorted list of SMBIOS type 11 strings for creds.
func renderCredentials(creds map[string]string) []string {
	names := make([]string, 0, len(creds))
	for n := range creds {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		v := creds[n]
		if printable(v) {
			out = append(out, credentialPrefix+n+"="+v)
		} else {
			out = append(out, credentialBinaryPrefix+n+"="+base64.StdEncoding.EncodeToString([]byte(v)))
		}
	}
	return out
}

// printable is true for single-line text (space and printable runes only).
func printable(v string) bool {
	for _, r := range v {
		if r != ' ' && !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// escape doubles commas, which is how QEMU option values carry them.
// EscapeOption escapes a value for use inside a comma-separated QEMU option
// string: QEMU reads ",," as a literal comma. Netdev.Backend builders must
// apply it to every embedded value.
func EscapeOption(v string) string { return strings.ReplaceAll(v, ",", ",,") }

func escape(v string) string { return EscapeOption(v) }

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", apierr.ErrInvalid, fmt.Sprintf(format, a...))
}

func (s Spec) validate() error {
	switch {
	case s.ID == "":
		return invalid("vm id is required")
	case s.Phase != PhaseInstall && s.Phase != PhaseBoot:
		return invalid("phase must be %q or %q, got %q", PhaseInstall, PhaseBoot, s.Phase)
	case s.CPUs < 1:
		return invalid("cpus must be at least 1")
	case s.MemoryMiB < 1:
		return invalid("memory must be at least 1 MiB")
	case s.Phase == PhaseInstall && s.UKI == "":
		return invalid("install phase needs a UKI")
	case s.Phase == PhaseInstall && s.Installer == "":
		return invalid("install phase needs the installer DDI")
	case s.Target == "":
		return invalid("target disk is required")
	case s.VsockCID < MinCID || s.VsockCID > MaxCID:
		return invalid("vsock cid must be in %d..%d, got %d", MinCID, MaxCID, s.VsockCID)
	case s.OVMFCode == "" || s.OVMFVars == "":
		return invalid("OVMF code and vars paths are required")
	case s.SerialLog == "":
		return invalid("serial log path is required")
	case s.QMPSocket == "":
		return invalid("QMP socket path is required")
	case len(s.QMPSocket) > MaxUnixSocketPath:
		return invalid("QMP socket path exceeds %d bytes; use a shorter state dir", MaxUnixSocketPath)
	case len(s.TPMSocket) > MaxUnixSocketPath:
		return invalid("TPM socket path exceeds %d bytes; use a shorter state dir", MaxUnixSocketPath)
	}
	serials := map[string]bool{SerialTarget: true}
	if s.Phase == PhaseInstall {
		serials[SerialInstaller] = true
	}
	for _, d := range s.ExtraDrives {
		if err := validateDrive(d, serials); err != nil {
			return err
		}
	}
	ids := map[string]bool{}
	for _, n := range s.Netdevs {
		if err := validateNetdev(n, ids); err != nil {
			return err
		}
	}
	for name := range s.Credentials {
		if err := validateCredentialName(name); err != nil {
			return err
		}
	}
	if strings.ContainsAny(s.KernelCmdlineExtra, "\n\x00") {
		return invalid("kernel cmdline extra must be a single line")
	}
	return nil
}

func validateDrive(d Drive, seen map[string]bool) error {
	switch {
	case d.Path == "":
		return invalid("drive %q has no path", d.Serial)
	case d.Format != "" && !driveFormats[d.Format]:
		return invalid("drive %q format %q must be raw or qcow2", d.Serial, d.Format)
	case d.Serial == "" || len(d.Serial) > virtioSerialMax || !serialPattern.MatchString(d.Serial):
		return invalid("drive serial %q must be 1-%d characters of [A-Za-z0-9._-]", d.Serial, virtioSerialMax)
	case seen[d.Serial]:
		return invalid("drive serial %q is used twice", d.Serial)
	}
	seen[d.Serial] = true
	return nil
}

func validateNetdev(n Netdev, seen map[string]bool) error {
	switch {
	case n.ID == "" || !serialPattern.MatchString(n.ID):
		return invalid("netdev id %q must be [A-Za-z0-9._-]", n.ID)
	case seen[n.ID]:
		return invalid("netdev id %q is used twice", n.ID)
	case n.Backend == "" || strings.Contains(n.Backend, "id="):
		return invalid("netdev %q backend must be set and carry no id", n.ID)
	case !netdevBackendPattern.MatchString(n.Backend):
		return invalid("netdev %q backend %q must start with the backend type, e.g. \"user\" or \"stream,addr.type=unix,...\"", n.ID, n.Backend)
	}
	if mac, err := net.ParseMAC(n.MAC); err != nil || len(mac) != 6 {
		return invalid("netdev %q mac %q is not a 48-bit address", n.ID, n.MAC)
	}
	seen[n.ID] = true
	return nil
}

// validateCredentialName follows systemd's credential_name_valid: a file
// name (no "/", not "." or ".."), printable ASCII, at most 255 bytes, and
// since it is rendered NAME=VALUE, no "=" (systemd also forbids ":").
func validateCredentialName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return invalid("credential name %q must be 1-255 characters and not . or ..", name)
	}
	for _, r := range name {
		if r < ' ' || r > '~' || strings.ContainsRune("/:=", r) {
			return invalid("credential name %q may only contain printable ASCII without / : =", name)
		}
	}
	return nil
}
