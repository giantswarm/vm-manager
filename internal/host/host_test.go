package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner answers by command name, or by name and first argument for a
// tool asked two different questions; unknown commands are "not found".
type fakeRunner map[string]string

func (f fakeRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	if len(args) > 0 {
		if out, ok := f[name+" "+args[0]]; ok {
			return out, nil
		}
	}
	out, ok := f[name]
	if !ok {
		return "", errors.New(name + ": executable file not found in $PATH")
	}
	return out, nil
}

// with is f plus the entries of more.
func (f fakeRunner) with(more fakeRunner) fakeRunner {
	out := fakeRunner{}
	for k, v := range f {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

var allTools = fakeRunner{
	QEMUBinary:  "QEMU emulator version 11.1.1\nCopyright (c) 2003-2025 Fabrice Bellard and the QEMU Project developers\n",
	SwtpmBinary: "TPM emulator version 0.10.2, Copyright (c) 2014-2022 IBM Corp. and others\n",
	"systemctl": "systemd 261 (261.3-1-arch)\n+PAM +AUDIT -SELINUX\n",
}

// Firmware locations of the fixtures: Arch's, and Ubuntu's with the Secure
// Boot build next to it, as ovmf-generic ships them.
const (
	archOVMF          = "/usr/share/edk2/x64/OVMF_CODE.4m.fd"
	ubuntuOVMF        = "/usr/share/OVMF/OVMF_CODE_4M.fd"
	ubuntuSecbootOVMF = "/usr/share/OVMF/OVMF_CODE_4M.secboot.fd"
)

// dpkgOVMF answers the two dpkg-query questions for ubuntuOVMF.
var dpkgOVMF = fakeRunner{
	DpkgQuery + " --search": "ovmf-generic: " + ubuntuOVMF + "\n",
	DpkgQuery + " --show":   "2025.11-3ubuntu7.2",
}

func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// fixture builds a fake root; each entry of files is created as a regular
// file with the given content.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	return root
}

// fullHost is a root where every prerequisite is present.
func fullHost(t *testing.T) string {
	t.Helper()
	return fixture(t, map[string]string{
		"dev/kvm":                                      "",
		"dev/vhost-vsock":                              "",
		"proc/sys/kernel/osrelease":                    "7.2.4-arch1-2\n",
		"proc/meminfo":                                 "MemTotal:       65536000 kB\nMemFree:        1000 kB\n",
		"usr/share/edk2/x64/OVMF_CODE.4m.fd":           "fw",
		"usr/share/edk2/x64/OVMF_CODE.secboot.4m.fd":   "secboot fw",
		"run/systemd/io.systemd.StorageProvider/fs":    "",
		"run/systemd/io.systemd.StorageProvider/block": "",
	})
}

func TestGet(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name   string
		root   func(t *testing.T) string
		ovmf   string // the configured code image, archOVMF when empty
		runner Runner
		check  func(t *testing.T, info *Info)
	}{
		{
			name:   "everything present",
			root:   fullHost,
			runner: allTools,
			check: func(t *testing.T, info *Info) {
				assert.True(t, info.Ready, "missing: %v", info.Missing)
				assert.Empty(t, info.Missing)
				assert.Equal(t, "7.2.4-arch1-2", info.Kernel)
				assert.Equal(t, uint64(65536000*1024), info.MemoryBytes)
				assert.Positive(t, info.CPUs)
				assert.NotEmpty(t, info.Hostname)
				assert.Equal(t, Device{Path: KVMDevice, Accessible: true}, info.KVM)
				assert.Equal(t, Device{Path: VsockDevice, Accessible: true}, info.VhostVsock)
				assert.Equal(t, Tool{Found: true, Version: "11.1.1"}, info.QEMU)
				assert.Equal(t, Tool{Found: true, Version: "0.10.2"}, info.Swtpm, "trailing comma dropped")
				assert.Equal(t, Tool{Found: true, Version: "261"}, info.Systemd)
				assert.Equal(t, archOVMF, info.OVMFCode, "the configured image, not the Secure Boot build next to it")
				require.NotNil(t, info.Firmware)
				assert.Equal(t, digest("fw"), info.Firmware.SHA256)
				assert.Empty(t, info.Firmware.Package, "no dpkg on this host")
				assert.Contains(t, info.Firmware.Error, "not found")
				assert.Equal(t, []string{"block", "fs"}, info.StorageProviders)
			},
		},
		{
			name:   "bare host",
			root:   func(t *testing.T) string { return t.TempDir() },
			runner: fakeRunner{},
			check: func(t *testing.T, info *Info) {
				assert.False(t, info.Ready)
				assert.Equal(t, []string{KVMDevice, VsockDevice, QEMUBinary, SwtpmBinary, "OVMF code image " + archOVMF}, info.Missing)
				assert.Empty(t, info.OVMFCode)
				assert.Nil(t, info.Firmware)
				assert.Equal(t, "", info.Kernel)
				assert.Equal(t, uint64(0), info.MemoryBytes)
				assert.Equal(t, "no such file or directory", info.KVM.Error, "path errors carry the reason only")
				assert.False(t, info.QEMU.Found)
				assert.Contains(t, info.QEMU.Error, "not found")
				assert.Equal(t, []string{}, info.StorageProviders, "an empty list, not null")
			},
		},
		{
			name: "swtpm missing",
			root: fullHost,
			runner: fakeRunner{
				QEMUBinary:  allTools[QEMUBinary],
				"systemctl": allTools["systemctl"],
			},
			check: func(t *testing.T, info *Info) {
				assert.False(t, info.Ready)
				assert.Equal(t, []string{SwtpmBinary}, info.Missing)
			},
		},
		{
			name: "ubuntu firmware names its package build, and only the fs provider",
			root: func(t *testing.T) string {
				return fixture(t, map[string]string{
					"dev/kvm":                      "",
					"dev/vhost-vsock":              "",
					ubuntuOVMF[1:]:                 "fw",
					ubuntuSecbootOVMF[1:]:          "secboot fw",
					StorageProviderDir[1:] + "/fs": "",
				})
			},
			ovmf:   ubuntuOVMF,
			runner: allTools.with(dpkgOVMF),
			check: func(t *testing.T, info *Info) {
				assert.True(t, info.Ready, "missing: %v", info.Missing)
				assert.Equal(t, ubuntuOVMF, info.OVMFCode)
				assert.Equal(t, &Firmware{SHA256: digest("fw"), Package: "ovmf-generic", Version: "2025.11-3ubuntu7.2"}, info.Firmware)
				assert.Equal(t, []string{"fs"}, info.StorageProviders)
			},
		},
		{
			name: "a package query dpkg cannot answer leaves the digest",
			root: func(t *testing.T) string {
				root := fullHost(t)
				require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(ubuntuOVMF)), 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(root, ubuntuOVMF), []byte("fw"), 0o600))
				return root
			},
			ovmf: ubuntuOVMF,
			runner: RunnerFunc(func(ctx context.Context, name string, args ...string) (string, error) {
				if name == DpkgQuery {
					return "", errors.New("dpkg-query: exit status 1: dpkg-query: no path found matching pattern " + ubuntuOVMF)
				}
				return allTools.Output(ctx, name, args...)
			}),
			check: func(t *testing.T, info *Info) {
				assert.True(t, info.Ready, "an unknown package does not block create_vm: %v", info.Missing)
				require.NotNil(t, info.Firmware)
				assert.Equal(t, digest("fw"), info.Firmware.SHA256)
				assert.Empty(t, info.Firmware.Package)
				assert.Contains(t, info.Firmware.Error, "no path found")
			},
		},
		{
			name: "a firmware path that is a directory is missing",
			root: func(t *testing.T) string {
				root := fixture(t, nil)
				require.NoError(t, os.MkdirAll(filepath.Join(root, ubuntuOVMF), 0o750))
				return root
			},
			ovmf:   ubuntuOVMF,
			runner: allTools,
			check: func(t *testing.T, info *Info) {
				assert.Contains(t, info.Missing, "OVMF code image "+ubuntuOVMF)
				assert.Nil(t, info.Firmware)
			},
		},
		{
			name: "without the fs provider the host is ready and reports what it has",
			root: func(t *testing.T) string {
				root := fullHost(t)
				require.NoError(t, os.Remove(filepath.Join(root, StorageProviderDir, "fs")))
				return root
			},
			runner: allTools,
			check: func(t *testing.T, info *Info) {
				assert.True(t, info.Ready, "missing: %v", info.Missing)
				assert.Equal(t, []string{"block"}, info.StorageProviders)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ovmf := tt.ovmf
			if ovmf == "" {
				ovmf = archOVMF
			}
			svc := New(Options{Runner: tt.runner, Root: tt.root(t), OVMFCode: ovmf, Logger: quiet})
			info, err := svc.Get(context.Background())
			require.NoError(t, err)
			tt.check(t, info)
		})
	}
}

func TestFirmwareUnconfigured(t *testing.T) {
	_, err := New(Options{Root: t.TempDir()}).Firmware(context.Background())
	require.Error(t, err)
	info, err := New(Options{Runner: allTools, Root: t.TempDir()}).Get(context.Background())
	require.NoError(t, err)
	assert.Contains(t, info.Missing, "OVMF code image")
}

func TestFieldAfter(t *testing.T) {
	assert.Equal(t, "11.1.1", fieldAfter("QEMU emulator version 11.1.1 (qemu-11.1.1-1.fc42)", "version"))
	assert.Equal(t, "261", fieldAfter("systemd 261 (261.3-1-arch)", "systemd"))
	assert.Equal(t, "", fieldAfter("version", "version"), "marker last")
	assert.Equal(t, "", fieldAfter("", "version"))
}

func TestExecRunner(t *testing.T) {
	out, err := ExecRunner{}.Output(context.Background(), "sh", "-c", "echo hello")
	require.NoError(t, err)
	assert.Equal(t, "hello\n", out)

	_, err = ExecRunner{}.Output(context.Background(), "vm-manager-no-such-binary")
	require.ErrorIs(t, err, exec.ErrNotFound)
	assert.Contains(t, err.Error(), "vm-manager-no-such-binary: ")

	_, err = ExecRunner{}.Output(context.Background(), "sh", "-c", "echo boom >&2; exit 3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom", "stderr is part of the error")
}

func TestDefaults(t *testing.T) {
	svc := New(Options{})
	assert.Equal(t, "/", svc.root)
	assert.IsType(t, ExecRunner{}, svc.run)
	assert.NotNil(t, svc.log)
}
