//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/giantswarm/vm-manager/internal/runtime/proc"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/tpm"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// sshPort is where the guest's systemd-ssh-generator binds sshd on AF_VSOCK.
const sshPort = 22

// Image artifacts: images/mkosi.conf sets ImageId= and
// UnifiedKernelImageFormat=%i_%v, so the files are <imageID>_<version>.{efi,raw}.
const (
	imageID     = "giantswarm-vm-base"
	imageDirEnv = "VM_MANAGER_E2E_IMAGE_DIR"
	keepEnv     = "VM_MANAGER_E2E_KEEP"
)

// kernelCmdlineExtra is appended to the UKI command line in both phases; see
// Harness.Spec.
const kernelCmdlineExtra = "systemd.imds=no"

// Sizing and limits.
const (
	// targetSize is the blank target disk: the installer layout needs 512M
	// ESP + 2x(2G+64M+4M) slots + >=1G var, so 8G leaves var ~3.3G.
	targetSize = 8 << 30
	// consoleTailLines is how much of a serial console a failure quotes.
	consoleTailLines = 60
	// maxTempBase keeps every unix socket path well under the 108-byte
	// sun_path limit even after the per-test suffix and file names.
	maxTempBase = 40
	// sshConnectTimeout bounds one ssh attempt.
	sshConnectTimeout = 10 * time.Second
	// sshRetryEvery paces SSHRetry.
	sshRetryEvery = time.Second
)

// Image is the set of artifacts one build of images/ produced.
type Image struct {
	// Dir is where the artifacts live.
	Dir string
	// Version is ImageVersion= (images/mkosi.version).
	Version string
	// UKI is the unified kernel image booted with -kernel in the installer
	// phase; DDI the base disk image the installer copies from.
	UKI string
	DDI string
	// RootHash is the verity root hash (hex), empty when the .roothash file
	// is absent. The root and verity partition UUIDs are its two halves.
	RootHash string
}

// Harness is the per-test VM stack: the state directory, the ssh key, the
// vsock CID, the notify listener and the factories for swtpm and QEMU. One
// Harness runs one VM at a time, in as many phases as the test needs; the
// TPM state, OVMF variables, target disk and machine ID carry over between
// them, which is what the two-phase install relies on.
type Harness struct {
	t   *testing.T
	log *slog.Logger

	// Dir is the state directory (short path: unix sockets live in it).
	Dir string
	// Image is what boots.
	Image Image
	// CID is the guest's vsock context id, shared by every phase.
	CID uint32
	// MachineID seeds /etc/machine-id in both phases (the var partition
	// UUID is derived from it).
	MachineID string
	// PrivateKey is the ed25519 key file (for a manual ssh into a kept
	// VM); signer is the same key as SSH authenticates root with;
	// AuthorizedKey is the matching authorized_keys line.
	PrivateKey    string
	AuthorizedKey string
	signer        ssh.Signer
	// OVMFCode is the host's firmware code image (vm.FindOVMF: Arch
	// edk2-ovmf or Debian/Ubuntu ovmf); OVMFVars the writable variable
	// store (bootctl writes the boot entry there in phase A, phase B boots
	// by it).
	OVMFCode string
	OVMFVars string
	// Target is the raw target disk.
	Target string
	// Notify receives READY=1 from the guest; Ready is its subscription
	// for CID.
	Notify *qemu.NotifyListener
	Ready  <-chan qemu.Notification

	tpmState string
	tpms     *tpm.Manager
	rt       *qemu.Runtime
}

// New prepares a Harness or skips the test with the reason the host cannot
// run it. Everything it creates is torn down by t.Cleanup; the state
// directory survives a failed test (and always with VM_MANAGER_E2E_KEEP=1).
func New(t *testing.T) *Harness {
	t.Helper()
	ovmfCode, ovmfVars := requireHost(t)
	image := locateImage(t)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	dir := stateDir(t)
	h := &Harness{
		t:         t,
		log:       log,
		Dir:       dir,
		Image:     image,
		CID:       allocateCID(),
		MachineID: randomHex(16),
		OVMFCode:  ovmfCode,
		OVMFVars:  filepath.Join(dir, "OVMF_VARS.fd"),
		Target:    filepath.Join(dir, "target.raw"),
		tpmState:  filepath.Join(dir, "tpm"),
		tpms:      tpm.New(tpm.Options{Logger: log}),
		rt:        qemu.New(qemu.Options{Logger: log, OVMFVarsTemplate: ovmfVars}),
	}
	h.PrivateKey, h.AuthorizedKey = generateSSHKey(t, dir)
	h.signer = loadSigner(t, h.PrivateKey)

	// A blank, sparse target: what a fresh volume from the storage layer
	// looks like to the installer.
	target, err := os.OpenFile(h.Target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(t, err)
	require.NoError(t, target.Truncate(targetSize))
	require.NoError(t, target.Close())

	nl, err := qemu.ListenNotify(0, log)
	require.NoError(t, err, "vsock notify listener (is /dev/vhost-vsock usable?)")
	t.Cleanup(func() { _ = nl.Close() })
	h.Notify = nl
	h.Ready = nl.Subscribe(h.CID)

	t.Logf("e2e state dir %s, vsock cid %d, machine id %s, image %s %s", dir, h.CID, h.MachineID, image.Version, image.Dir)
	return h
}

// requireHost skips unless KVM, vsock, the binaries and the firmware are
// there, and returns the firmware pair (code image, variable store
// template) the host has: what `vm-manager serve` picks by default too.
func requireHost(t *testing.T) (ovmfCode, ovmfVars string) {
	t.Helper()
	for _, dev := range []string{"/dev/kvm", "/dev/vhost-vsock"} {
		f, err := os.OpenFile(dev, os.O_RDWR, 0) // #nosec G304 -- fixed device paths
		if err != nil {
			t.Skipf("e2e: %v", err)
		}
		_ = f.Close()
	}
	for _, bin := range []string{qemu.DefaultBinary, tpm.DefaultBinary, "sfdisk"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("e2e: %s not installed", bin)
		}
	}
	ovmfCode, ovmfVars = vm.FindOVMF()
	for _, p := range []string{ovmfCode, ovmfVars} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("e2e: OVMF firmware: %v", err)
		}
	}
	return ovmfCode, ovmfVars
}

// locateImage finds the artifacts in $VM_MANAGER_E2E_IMAGE_DIR or
// <repo>/images/build, with the version from images/mkosi.version (falling
// back to the single <imageID>_*.efi in the directory), or skips.
func locateImage(t *testing.T) Image {
	t.Helper()
	root := repoRoot(t)
	dir := os.Getenv(imageDirEnv)
	if dir == "" {
		dir = filepath.Join(root, "images", "build")
	}
	version := ""
	if b, err := os.ReadFile(filepath.Join(root, "images", "mkosi.version")); err == nil { // #nosec G304 -- repo file
		version = strings.TrimSpace(string(b))
	}
	if version == "" {
		ukis, _ := filepath.Glob(filepath.Join(dir, imageID+"_*.efi"))
		if len(ukis) != 1 {
			t.Skipf("e2e: no images/mkosi.version and %d UKIs in %s; run `make -C images keys base` or set %s", len(ukis), dir, imageDirEnv)
		}
		version = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(ukis[0]), imageID+"_"), ".efi")
	}
	img := Image{
		Dir:     dir,
		Version: version,
		UKI:     filepath.Join(dir, imageID+"_"+version+".efi"),
		DDI:     filepath.Join(dir, imageID+"_"+version+".raw"),
	}
	for _, p := range []string{img.UKI, img.DDI} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("e2e: image artifact missing: %v; run `make -C images keys base` or set %s", err, imageDirEnv)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, imageID+"_"+version+".roothash")); err == nil { // #nosec G304 G703 -- build artifact under the configured image dir
		img.RootHash = strings.TrimSpace(string(b))
	}
	return img
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("e2e: no go.mod above %s", dir)
		}
		dir = parent
	}
}

// stateDir creates the per-test directory under a short base so that the
// unix sockets in it (swtpm control, QMP) fit sun_path. It is removed on
// success unless VM_MANAGER_E2E_KEEP is set.
func stateDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if len(base) > maxTempBase {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "vmm-e2e-")
	require.NoError(t, err)
	t.Cleanup(func() {
		if t.Failed() || os.Getenv(keepEnv) != "" {
			t.Logf("e2e: keeping state dir %s", dir)
			return
		}
		_ = os.RemoveAll(dir)
	})
	return dir
}

// allocateCID picks a vsock CID well above the range vmspawn and other
// tools derive from machine names; QEMU refuses one that is in use.
func allocateCID() uint32 {
	return 0x100000 + mathrand.Uint32N(0x7000_0000) // #nosec G404 -- an id, not a secret
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// generateSSHKey writes an ed25519 key to dir/id_ed25519 (0600) and returns
// its path and the authorized_keys line.
func generateSSHKey(t *testing.T, dir string) (string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "vm-manager e2e")
	require.NoError(t, err)
	path := filepath.Join(dir, "id_ed25519")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0o600))
	sshPub, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return path, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

// loadSigner reads the key generateSSHKey wrote, for SSH.
func loadSigner(t *testing.T, path string) ssh.Signer {
	t.Helper()
	pemBytes, err := os.ReadFile(path) // #nosec G304 -- the key file this harness wrote
	require.NoError(t, err)
	signer, err := ssh.ParsePrivateKey(pemBytes)
	require.NoError(t, err)
	return signer
}

// StartTPM starts swtpm on the harness's TPM state; the state directory is
// the same in every phase, the process is not: swtpm terminates with the
// QEMU it served, so each phase gets a fresh one over the persisted state.
func (h *Harness) StartTPM(ctx context.Context) *tpm.Instance {
	h.t.Helper()
	inst, err := h.tpms.Start(ctx, tpm.Config{StateDir: h.tpmState})
	require.NoError(h.t, err)
	h.t.Cleanup(func() { _ = inst.Kill() })
	return inst
}

// Spec is the VM description shared by both phases: the target disk, user
// networking, the vTPM, the firmware pair, the notify credential and the
// machine ID. name selects the serial log (<name>.log) and QMP socket.
// Callers add the phase's own credentials and, for PhaseInstall, NoReboot.
func (h *Harness) Spec(phase qemu.Phase, tp *tpm.Instance, name string) qemu.Spec {
	spec := qemu.Spec{
		ID:        "e2e-" + name,
		Phase:     phase,
		CPUs:      2,
		MemoryMiB: 2048,
		Target:    h.Target,
		Netdevs:   []qemu.Netdev{{ID: "net0", Backend: "user", MAC: "52:54:00:e2:e0:01"}},
		Credentials: map[string]string{
			qemu.CredentialNotifySocket: h.Notify.Credential(),
			qemu.CredentialMachineID:    h.MachineID,
		},
		// User networking has no IMDS at 169.254.169.254, and the image's
		// hwdb record makes the guest query one in the initrd and again
		// in the host system, each failing after a network timeout
		// (~45 s) before vm-sysinstall.service and READY=1 may proceed.
		// Until the network e2e serves a real IMDS, the IMDS logic is
		// switched off (systemd-imds-generator(8) systemd.imds=).
		KernelCmdlineExtra: kernelCmdlineExtra,
		VsockCID:           h.CID,
		TPMSocket:          tp.SocketPath(),
		OVMFCode:           h.OVMFCode,
		OVMFVars:           h.OVMFVars,
		SerialLog:          filepath.Join(h.Dir, name+".log"),
		QMPSocket:          filepath.Join(h.Dir, name+".qmp"),
	}
	if phase == qemu.PhaseInstall {
		spec.UKI = h.Image.UKI
		spec.Installer = h.Image.DDI
		spec.NoReboot = true
		spec.Credentials[qemu.CredentialInstallTarget] = qemu.InstallTargetDevice
	}
	return spec
}

// Start launches QEMU for spec and registers a kill for cleanup. Ready
// notifications left over from an earlier phase are discarded first so a
// READY=1 the installer boot sent cannot be mistaken for the installed
// system's.
func (h *Harness) Start(ctx context.Context, spec qemu.Spec) *qemu.Instance {
	h.t.Helper()
	for drained := false; !drained; {
		select {
		case n := <-h.Ready:
			h.t.Logf("discarding stale notification %v", n.Fields)
		default:
			drained = true
		}
	}
	inst, err := h.rt.Start(ctx, spec)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { _ = inst.Kill() })
	h.t.Logf("%s phase started: qemu pid %d, console %s", spec.Phase, inst.PID(), spec.SerialLog)
	return inst
}

// WaitExit fails the test, quoting the console, unless QEMU ends within
// the ceiling. Notifications the guest sends meanwhile (PID 1 streams
// STATUS= updates through boot) are consumed so the subscription never
// overflows.
func (h *Harness) WaitExit(inst *qemu.Instance, within time.Duration) proc.ExitStatus {
	h.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case st := <-inst.Wait():
			return st
		case n := <-h.Ready:
			h.noteNotification(n)
		case <-deadline:
			h.t.Fatalf("qemu (%s phase) still running after %s; stderr: %q\n%s", inst.Spec().Phase, within, inst.Stderr(), h.ConsoleTail(inst))
			return proc.ExitStatus{}
		}
	}
}

// noteNotification logs one sd_notify message from the guest; READY=1 is
// worth a test log line, the STATUS= stream only the debug log.
func (h *Harness) noteNotification(n qemu.Notification) {
	h.t.Helper()
	if n.Ready() {
		h.t.Logf("guest cid %d port %d: READY=1 %v", n.CID, n.Port, n.Fields)
		return
	}
	h.log.Debug("guest notification", "cid", n.CID, "port", n.Port, "fields", n.Fields)
}

// WaitReady returns the first READY=1 from the guest, failing the test with
// the console when QEMU exits or the ceiling passes first.
func (h *Harness) WaitReady(inst *qemu.Instance, within time.Duration) qemu.Notification {
	h.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case n, ok := <-h.Ready:
			if !ok {
				h.t.Fatalf("notify subscription closed before READY=1\n%s", h.ConsoleTail(inst))
			}
			h.noteNotification(n)
			if n.Ready() {
				return n
			}
		case st := <-inst.Wait():
			h.t.Fatalf("qemu exited (%s) before READY=1; stderr: %q\n%s", st, inst.Stderr(), h.ConsoleTail(inst))
		case <-deadline:
			h.t.Fatalf("no READY=1 within %s\n%s", within, h.ConsoleTail(inst))
		}
	}
}

// Console is the whole serial log of inst so far.
func (h *Harness) Console(inst *qemu.Instance) string {
	h.t.Helper()
	b, err := os.ReadFile(inst.Spec().SerialLog)
	if err != nil {
		return fmt.Sprintf("(console unreadable: %v)", err)
	}
	return string(b)
}

// ConsoleTail is the last consoleTailLines lines of the serial log,
// labelled, for failure messages.
func (h *Harness) ConsoleTail(inst *qemu.Instance) string {
	h.t.Helper()
	return fmt.Sprintf("--- last %d console lines of %s ---\n%s\n--- end console ---",
		consoleTailLines, inst.Spec().SerialLog, tail(h.Console(inst), consoleTailLines))
}

// tail keeps the last n lines of s, without carriage returns.
func tail(s string, n int) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r", ""), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// SSH runs command as root in the guest over AF_VSOCK (the guest's
// systemd-ssh-generator binds sshd to vsock port 22) and returns stdout;
// stderr is part of the error. The connection is dialed from Go, so the host
// needs neither an ssh client nor systemd-ssh-proxy (systemd 256+), which
// hosted CI runners lack. One call is bounded by 2*sshConnectTimeout.
func (h *Harness) SSH(ctx context.Context, command string) (string, error) {
	h.t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 2*sshConnectTimeout)
	defer cancel()
	client, err := h.dialSSH(cctx)
	if err != nil {
		return "", fmt.Errorf("ssh %q: %w", command, err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh %q: session: %w", command, err)
	}
	defer func() { _ = session.Close() }()
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	if err := session.Run(command); err != nil {
		return stdout.String(), fmt.Errorf("ssh %q: %w: %s", command, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// dialSSH connects to the guest's sshd over vsock and completes the ssh
// handshake as root with the harness key. The deadline of ctx becomes the
// connection's, so the whole call (handshake and command) ends with it. The
// host key is not checked: the guest generates it on first boot and vsock
// addresses only this VM.
func (h *Harness) dialSSH(ctx context.Context) (*ssh.Client, error) {
	conn, err := vsock.Dial(h.CID, sshPort, nil)
	if err != nil {
		return nil, fmt.Errorf("vsock cid %d port %d: %w", h.CID, sshPort, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(h.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // #nosec G106 -- a throw-away test VM reached over vsock, see above
		Timeout:         sshConnectTimeout,
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, fmt.Sprintf("vsock/%d", h.CID), cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// SSHRetry repeats SSH until it succeeds or within passes: sshd's host keys
// are generated on the first boot and the vsock listener may come up a
// moment after READY=1.
func (h *Harness) SSHRetry(ctx context.Context, within time.Duration, command string) string {
	h.t.Helper()
	deadline := time.Now().Add(within)
	var lastErr error
	for attempt := 1; ; attempt++ {
		out, err := h.SSH(ctx, command)
		if err == nil {
			h.t.Logf("ssh %q succeeded on attempt %d", command, attempt)
			return out
		}
		lastErr = err
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(sshRetryEvery)
	}
	h.t.Fatalf("ssh over vsock did not succeed within %s: %v", within, lastErr)
	return ""
}

// Partition is one GPT entry as sfdisk --json reports it.
type Partition struct {
	Node  string `json:"node"`
	Start int64  `json:"start"`
	Size  int64  `json:"size"`
	Type  string `json:"type"`
	UUID  string `json:"uuid"`
	Name  string `json:"name"`
}

// SizeKiB is the partition size in KiB (512-byte sectors).
func (p Partition) SizeKiB() int64 { return p.Size * 512 / 1024 }

// Number is the GPT partition number, the digits sfdisk appends to the
// image path in Node; 0 when there are none.
func (p Partition) Number() int {
	i := len(p.Node)
	for i > 0 && p.Node[i-1] >= '0' && p.Node[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(p.Node[i:])
	return n
}

// PartitionTable reads the GPT of a raw image with sfdisk --json.
func PartitionTable(t *testing.T, image string) []Partition {
	t.Helper()
	out, err := exec.Command("sfdisk", "--json", image).Output() // #nosec G204 -- fixed binary
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("sfdisk --json %s: %v: %s", image, err, exitErr.Stderr)
	}
	require.NoError(t, err)
	var table struct {
		PartitionTable struct {
			Label      string      `json:"label"`
			SectorSize int         `json:"sectorsize"`
			Partitions []Partition `json:"partitions"`
		} `json:"partitiontable"`
	}
	require.NoError(t, json.Unmarshal(out, &table), "sfdisk output: %s", out)
	require.Equal(t, "gpt", table.PartitionTable.Label, "partition table label")
	require.Equal(t, 512, table.PartitionTable.SectorSize, "sector size")
	return table.PartitionTable.Partitions
}

// Describe renders a partition table the way a failure message wants it.
func Describe(parts []Partition) string {
	var b strings.Builder
	for _, p := range parts {
		fmt.Fprintf(&b, "  %2d %10d KiB  %s  %s  %s\n", p.Number(), p.SizeKiB(), p.Type, p.UUID, p.Name)
	}
	return b.String()
}
