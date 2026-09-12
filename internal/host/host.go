// Package host reports what the KVM host offers vm-manager: kernel and
// hardware, the devices and tools the QEMU runtime needs, the firmware the
// images boot with, and the systemd services the provisioner talks to. It is
// the first thing an agent calls: get_host says whether VMs can be created
// here and, if not, what is missing.
package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Paths probed on the host, relative to Options.Root.
const (
	KVMDevice          = "/dev/kvm"
	VsockDevice        = "/dev/vhost-vsock"
	StorageProviderDir = "/run/systemd/io.systemd.StorageProvider"
	kernelReleaseFile  = "/proc/sys/kernel/osrelease"
	meminfoFile        = "/proc/meminfo"
)

// Tools probed on PATH.
const (
	QEMUBinary  = "qemu-system-x86_64"
	SwtpmBinary = "swtpm"
)

// StorageProviderFS is the systemd storage provider VM volumes come from.
const StorageProviderFS = "fs"

// ovmfCodeCandidates are the OVMF firmware code images, in preference order:
// the 4 MiB Secure Boot capable builds first, Arch (edk2-ovmf) then Debian
// and Ubuntu (ovmf) locations.
var ovmfCodeCandidates = []string{
	"/usr/share/edk2/x64/OVMF_CODE.secboot.4m.fd",
	"/usr/share/edk2/x64/OVMF_CODE.4m.fd",
	"/usr/share/OVMF/OVMF_CODE_4M.secboot.fd",
	"/usr/share/OVMF/OVMF_CODE_4M.fd",
	"/usr/share/OVMF/OVMF_CODE.secboot.fd",
	"/usr/share/OVMF/OVMF_CODE.fd",
}

// probeTimeout bounds each tool invocation.
const probeTimeout = 5 * time.Second

// Runner runs a host command; tests inject a fake.
type Runner interface {
	// Output runs name with args and returns its standard output.
	Output(ctx context.Context, name string, args ...string) (string, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, name string, args ...string) (string, error)

// Output implements Runner.
func (f RunnerFunc) Output(ctx context.Context, name string, args ...string) (string, error) {
	return f(ctx, name, args...)
}

// ExecRunner runs commands on the host through os/exec.
type ExecRunner struct{}

// Output implements Runner. A missing binary and a non-zero exit both come
// back as errors that name the command; stderr is appended on exit errors.
func (ExecRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output() // #nosec G204 -- command names are constants of this package
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// Options configure the Service.
type Options struct {
	// Runner executes host tools; nil uses ExecRunner.
	Runner Runner
	// Root is the filesystem root the device, firmware and /proc probes are
	// relative to; empty is "/". Tests point it at a fixture tree.
	Root string
	// Logger for probe failures; nil uses slog.Default().
	Logger *slog.Logger
}

// Service answers host capability queries.
type Service struct {
	run  Runner
	root string
	log  *slog.Logger
}

// New builds a Service.
func New(opts Options) *Service {
	s := &Service{run: opts.Runner, root: opts.Root, log: opts.Logger}
	if s.run == nil {
		s.run = ExecRunner{}
	}
	if s.root == "" {
		s.root = "/"
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Info is the host capability report, the body of GET /api/v1/host and the
// get_host tool result.
type Info struct {
	// Hostname is the kernel hostname.
	Hostname string `json:"hostname"`
	// Kernel is the running kernel release.
	Kernel string `json:"kernel"`
	// CPUs is the number of logical CPUs available to vm-manager.
	CPUs int `json:"cpus"`
	// MemoryBytes is MemTotal from /proc/meminfo.
	MemoryBytes uint64 `json:"memoryBytes"`
	// KVM is /dev/kvm, the hardware virtualization device.
	KVM Device `json:"kvm"`
	// VhostVsock is /dev/vhost-vsock, the guest notification channel.
	VhostVsock Device `json:"vhostVsock"`
	// QEMU is the qemu-system-x86_64 binary.
	QEMU Tool `json:"qemu"`
	// Swtpm is the swtpm binary that backs each VM's vTPM.
	Swtpm Tool `json:"swtpm"`
	// Systemd is the host systemd, which provides the storage provider and
	// the transient units VMs run in.
	Systemd Tool `json:"systemd"`
	// OVMFCode is the UEFI firmware code image VMs boot with; empty when no
	// known location holds one.
	OVMFCode string `json:"ovmfCode,omitempty"`
	// StorageProviders are the io.systemd.StorageProvider sockets present.
	StorageProviders []string `json:"storageProviders"`
	// Ready is true when every prerequisite of create_vm is met.
	Ready bool `json:"ready"`
	// Missing names the prerequisites that are not met, in the order they
	// are checked; empty when Ready.
	Missing []string `json:"missing,omitempty"`
}

// Device is a host device node and whether this process may open it.
type Device struct {
	Path       string `json:"path"`
	Accessible bool   `json:"accessible"`
	Error      string `json:"error,omitempty"`
}

// Tool is a host binary and the version it reports.
type Tool struct {
	Found   bool   `json:"found"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Get probes the host. Individual prerequisites that are absent are reported
// in the result, not as an error; an error is a failure to read the host at
// all.
func (s *Service) Get(ctx context.Context) (*Info, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("hostname: %w", err)
	}
	info := &Info{
		Hostname:         hostname,
		Kernel:           s.kernel(),
		CPUs:             runtime.NumCPU(),
		MemoryBytes:      s.memTotal(),
		KVM:              s.device(KVMDevice),
		VhostVsock:       s.device(VsockDevice),
		QEMU:             s.tool(ctx, QEMUBinary, "--version", "version"),
		Swtpm:            s.tool(ctx, SwtpmBinary, "--version", "version"),
		Systemd:          s.tool(ctx, "systemctl", "--version", "systemd"),
		OVMFCode:         s.ovmfCode(),
		StorageProviders: s.storageProviders(),
	}
	info.Missing = missing(info)
	info.Ready = len(info.Missing) == 0
	return info, nil
}

// missing lists the create_vm prerequisites info does not satisfy.
func missing(info *Info) []string {
	var out []string
	if !info.KVM.Accessible {
		out = append(out, KVMDevice)
	}
	if !info.VhostVsock.Accessible {
		out = append(out, VsockDevice)
	}
	if !info.QEMU.Found {
		out = append(out, QEMUBinary)
	}
	if !info.Swtpm.Found {
		out = append(out, SwtpmBinary)
	}
	if info.OVMFCode == "" {
		out = append(out, "OVMF code image")
	}
	if !info.Systemd.Found {
		out = append(out, "systemd")
	}
	if !slices.Contains(info.StorageProviders, StorageProviderFS) {
		out = append(out, "storage provider "+StorageProviderFS)
	}
	return out
}

func (s *Service) path(p string) string { return filepath.Join(s.root, p) }

func (s *Service) kernel() string {
	b, err := os.ReadFile(s.path(kernelReleaseFile))
	if err != nil {
		s.log.Debug("kernel release unavailable", "error", err)
		return ""
	}
	return strings.TrimSpace(string(b))
}

// memTotal is MemTotal from /proc/meminfo in bytes, 0 when unreadable.
func (s *Service) memTotal() uint64 {
	f, err := os.Open(s.path(meminfoFile))
	if err != nil {
		s.log.Debug("meminfo unavailable", "error", err)
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kib * 1024
	}
	return 0
}

// device reports whether p can be opened read-write, which is what QEMU and
// the vsock listener need.
func (s *Service) device(p string) Device {
	d := Device{Path: p}
	f, err := os.OpenFile(s.path(p), os.O_RDWR, 0)
	if err != nil {
		d.Error = pathErr(err)
		return d
	}
	_ = f.Close()
	d.Accessible = true
	return d
}

// tool runs name with arg and takes the version as the field after marker on
// the first output line ("QEMU emulator version 11.1.1", "systemd 261 (...)").
func (s *Service) tool(ctx context.Context, name, arg, marker string) Tool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := s.run.Output(ctx, name, arg)
	if err != nil {
		s.log.Debug("tool probe failed", "tool", name, "error", err)
		return Tool{Error: err.Error()}
	}
	return Tool{Found: true, Version: fieldAfter(firstLine(out), marker)}
}

func (s *Service) ovmfCode() string {
	for _, c := range ovmfCodeCandidates {
		if st, err := os.Stat(s.path(c)); err == nil && st.Mode().IsRegular() {
			return c
		}
	}
	return ""
}

// storageProviders are the non-directory entries (the provider sockets) of
// the io.systemd.StorageProvider directory, sorted by name.
func (s *Service) storageProviders() []string {
	entries, err := os.ReadDir(s.path(StorageProviderDir))
	if err != nil {
		s.log.Debug("storage provider directory unavailable", "error", err)
		return []string{}
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// fieldAfter is the whitespace-separated field following marker, trailing
// punctuation dropped; "" when marker is absent or last.
func fieldAfter(line, marker string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if f == marker && i+1 < len(fields) {
			return strings.TrimRight(fields[i+1], ",;")
		}
	}
	return ""
}

// pathErr is the reason of a *fs.PathError without the path, which the
// caller already has.
func pathErr(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
