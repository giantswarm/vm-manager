// Package host reports what the KVM host offers vm-manager: kernel and
// hardware, the devices and tools the QEMU runtime needs, the firmware the
// VMs boot with and its build, and the systemd services the provisioner
// talks to. It is the first thing an agent calls: get_host says whether VMs
// can be created here and, if not, what is missing.
package host

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	// DpkgQuery names the package that installed the firmware image.
	DpkgQuery = "dpkg-query"
)

// StorageProviderFS is the systemd storage provider VM volumes come from.
const StorageProviderFS = "fs"

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
	// OVMFCode is the firmware code image VMs boot with, serve's
	// --ovmf-code.
	OVMFCode string
	// Logger for probe failures; nil uses slog.Default().
	Logger *slog.Logger
}

// Service answers host capability queries.
type Service struct {
	run      Runner
	root     string
	ovmfCode string
	log      *slog.Logger
}

// New builds a Service.
func New(opts Options) *Service {
	s := &Service{run: opts.Runner, root: opts.Root, ovmfCode: opts.OVMFCode, log: opts.Logger}
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
	// OVMFCode is the UEFI firmware code image VMs boot with (serve's
	// --ovmf-code); empty when that file does not exist.
	OVMFCode string `json:"ovmfCode,omitempty"`
	// Firmware is the build of OVMFCode; absent with it.
	Firmware *Firmware `json:"firmware,omitempty"`
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

// Firmware identifies the build of the firmware code image. PCR 0 measures
// the firmware, so golden PCR values recorded under one build fail to verify
// under any other: the build a policy's values were recorded with is the one
// this must report.
type Firmware struct {
	// SHA256 is the hex digest of the code image, the build's exact identity
	// on any host.
	SHA256 string `json:"sha256"`
	// Package and Version name the dpkg package that installed the image,
	// e.g. ovmf-generic 2025.11-3ubuntu7.2 in the vm-manager container image;
	// empty on a host whose image dpkg does not know, Error says why.
	Package string `json:"package,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
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
		StorageProviders: s.storageProviders(),
	}
	if fw, err := s.Firmware(ctx); err != nil {
		s.log.Debug("firmware code image unavailable", "path", s.ovmfCode, "error", err)
	} else {
		info.OVMFCode, info.Firmware = s.ovmfCode, fw
	}
	info.Missing = s.missing(info)
	info.Ready = len(info.Missing) == 0
	return info, nil
}

// missing lists the create_vm prerequisites info does not satisfy: the two
// devices, the two binaries and the firmware image. systemd and the fs storage
// provider are reported (Systemd, StorageProviders) but not required: without
// a service manager VMs run as plain child processes (--launcher process), and
// without the provider volumes are plain files below the state directory — the
// shape of vm-manager in a pod.
func (s *Service) missing(info *Info) []string {
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
		out = append(out, strings.TrimSpace("OVMF code image "+s.ovmfCode))
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

// Firmware reads the build of the firmware code image VMs boot with: its
// digest, and the dpkg package and version that installed it when dpkg
// knows the file. The error is a missing or unreadable image.
func (s *Service) Firmware(ctx context.Context) (*Firmware, error) {
	if s.ovmfCode == "" {
		return nil, errors.New("no firmware code image configured")
	}
	st, err := os.Stat(s.path(s.ovmfCode))
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", s.ovmfCode)
	}
	f, err := os.Open(s.path(s.ovmfCode)) // #nosec G304 -- the configured firmware image
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("read %s: %w", s.ovmfCode, err)
	}
	fw := &Firmware{SHA256: hex.EncodeToString(h.Sum(nil))}
	if fw.Package, fw.Version, err = s.dpkgPackage(ctx, s.ovmfCode); err != nil {
		fw.Error = err.Error()
	}
	return fw, nil
}

// dpkgPackage is the package that installed path and its version:
// dpkg-query --search prints "ovmf-generic: /usr/share/OVMF/OVMF_CODE_4M.fd"
// (a comma-separated list when several packages ship the path, the first
// wins), --show the version of that package.
func (s *Service) dpkgPackage(ctx context.Context, path string) (pkg, version string, err error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := s.run.Output(ctx, DpkgQuery, "--search", path)
	if err != nil {
		return "", "", err
	}
	owners, _, ok := strings.Cut(firstLine(out), ": ")
	if !ok {
		return "", "", fmt.Errorf("%s --search %s: unexpected output %q", DpkgQuery, path, firstLine(out))
	}
	pkg, _, _ = strings.Cut(owners, ", ")
	out, err = s.run.Output(ctx, DpkgQuery, "--show", "--showformat=${Version}", pkg)
	if err != nil {
		return "", "", err
	}
	return pkg, strings.TrimSpace(out), nil
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
