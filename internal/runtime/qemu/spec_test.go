package qemu

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

// installSpec is the phase A spec from docs/design.md "Boot flow".
func installSpec() Spec {
	return Spec{
		ID:        "vm-1",
		Phase:     PhaseInstall,
		CPUs:      2,
		MemoryMiB: 2048,
		UKI:       "/var/lib/vm-manager/images/base_1.efi",
		Installer: "/var/lib/vm-manager/images/base_1.raw",
		Target:    "/var/lib/vm-manager/volumes/vm-1.raw",
		Netdevs: []Netdev{{
			ID:      "net0",
			Backend: "stream,addr.type=unix,addr.path=/run/vm-manager/net/vm-1.sock,reconnect-ms=1000",
			MAC:     "52:54:00:aa:bb:01",
		}},
		Credentials: map[string]string{ //nolint:gosec // G101: fixture values, not secrets
			CredentialNotifySocket:          "vsock-stream:2:40001",
			CredentialInstallTarget:         InstallTargetDevice,
			CredentialHostname:              "node-1",
			CredentialMachineID:             "0123456789abcdef0123456789abcdef",
			CredentialSSHAuthorizedKeysRoot: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample root@host",
		},
		VsockCID:  4001,
		TPMSocket: "/run/vm-manager/vm-1/swtpm.sock",
		OVMFCode:  "/usr/share/edk2/x64/OVMF_CODE.4m.fd",
		OVMFVars:  "/var/lib/vm-manager/vms/vm-1/OVMF_VARS.fd",
		SerialLog: "/var/lib/vm-manager/vms/vm-1/serial.log",
		QMPSocket: "/run/vm-manager/vm-1/qmp.sock",
		NoReboot:  true,
	}
}

// bootSpec is the first phase B boot of the same VM.
func bootSpec() Spec {
	s := installSpec()
	s.Phase = PhaseBoot
	s.UKI, s.Installer = "", ""
	s.NoReboot = false
	s.KernelCmdlineExtra = "ignition.firstboot"
	s.Credentials = map[string]string{
		"vmm.notify_socket": "vsock-stream:2:40001",
		"system.machine_id": "0123456789abcdef0123456789abcdef",
	}
	s.ExtraDrives = []Drive{{Path: "/var/lib/vm-manager/volumes/vm-1-data.qcow2", Serial: "data0", Format: "qcow2"}}
	return s
}

func TestCommand(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want []string
	}{
		{
			name: "install phase",
			spec: installSpec(),
			want: []string{
				"-name", "guest=vm-1",
				"-machine", "q35",
				"-accel", "kvm",
				"-cpu", "host",
				"-smp", "2",
				"-m", "2048M",
				"-nodefaults",
				"-display", "none",
				"-serial", "file:/var/lib/vm-manager/vms/vm-1/serial.log",
				"-qmp", "unix:/run/vm-manager/vm-1/qmp.sock,server=on,wait=off",
				"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=/usr/share/edk2/x64/OVMF_CODE.4m.fd",
				"-drive", "if=pflash,format=raw,unit=1,file=/var/lib/vm-manager/vms/vm-1/OVMF_VARS.fd",
				"-kernel", "/var/lib/vm-manager/images/base_1.efi",
				"-drive", "if=none,id=installer,format=raw,file=/var/lib/vm-manager/images/base_1.raw,readonly=on",
				"-device", "virtio-blk-pci,drive=installer,serial=installer",
				"-drive", "if=none,id=target,format=raw,file=/var/lib/vm-manager/volumes/vm-1.raw,discard=unmap",
				"-device", "virtio-blk-pci,drive=target,serial=target",
				"-netdev", "stream,addr.type=unix,addr.path=/run/vm-manager/net/vm-1.sock,reconnect-ms=1000,id=net0",
				"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:aa:bb:01",
				"-device", "vhost-vsock-pci,id=vsock0,guest-cid=4001",
				"-chardev", "socket,id=chrtpm,path=/run/vm-manager/vm-1/swtpm.sock",
				"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
				"-device", "tpm-crb,tpmdev=tpm0",
				"-smbios", "type=1,manufacturer=GiantSwarm,product=vm-manager,serial=vm-1",
				"-smbios", "type=11,value=io.systemd.credential:firstboot.hostname=node-1",
				"-smbios", "type=11,value=io.systemd.credential:ssh.authorized_keys.root=ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample root@host",
				"-smbios", "type=11,value=io.systemd.credential:system.machine_id=0123456789abcdef0123456789abcdef",
				"-smbios", "type=11,value=io.systemd.credential:vm.install-target=/dev/disk/by-id/virtio-target",
				"-smbios", "type=11,value=io.systemd.credential:vmm.notify_socket=vsock-stream:2:40001",
				"-no-reboot",
			},
		},
		{
			name: "first installed boot",
			spec: bootSpec(),
			want: []string{
				"-name", "guest=vm-1",
				"-machine", "q35",
				"-accel", "kvm",
				"-cpu", "host",
				"-smp", "2",
				"-m", "2048M",
				"-nodefaults",
				"-display", "none",
				"-serial", "file:/var/lib/vm-manager/vms/vm-1/serial.log",
				"-qmp", "unix:/run/vm-manager/vm-1/qmp.sock,server=on,wait=off",
				"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=/usr/share/edk2/x64/OVMF_CODE.4m.fd",
				"-drive", "if=pflash,format=raw,unit=1,file=/var/lib/vm-manager/vms/vm-1/OVMF_VARS.fd",
				"-drive", "if=none,id=target,format=raw,file=/var/lib/vm-manager/volumes/vm-1.raw,discard=unmap",
				"-device", "virtio-blk-pci,drive=target,serial=target,bootindex=0",
				"-drive", "if=none,id=data0,format=qcow2,file=/var/lib/vm-manager/volumes/vm-1-data.qcow2,discard=unmap",
				"-device", "virtio-blk-pci,drive=data0,serial=data0",
				"-netdev", "stream,addr.type=unix,addr.path=/run/vm-manager/net/vm-1.sock,reconnect-ms=1000,id=net0",
				"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:aa:bb:01",
				"-device", "vhost-vsock-pci,id=vsock0,guest-cid=4001",
				"-chardev", "socket,id=chrtpm,path=/run/vm-manager/vm-1/swtpm.sock",
				"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
				"-device", "tpm-crb,tpmdev=tpm0",
				"-smbios", "type=1,manufacturer=GiantSwarm,product=vm-manager,serial=vm-1",
				"-smbios", "type=11,value=io.systemd.credential:system.machine_id=0123456789abcdef0123456789abcdef",
				"-smbios", "type=11,value=io.systemd.credential:vmm.notify_socket=vsock-stream:2:40001",
				"-smbios", "type=11,value=io.systemd.stub.kernel-cmdline-extra=ignition.firstboot",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Command(tc.spec)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCommandDetails(t *testing.T) {
	t.Run("binary credential and comma escaping", func(t *testing.T) {
		s := bootSpec()
		s.ID = "a,b"
		s.SMBIOS = SMBIOS{Manufacturer: "Acme, Inc", Product: "box", Version: "2", Serial: "SN1"}
		s.Credentials = map[string]string{
			"multi":  "line1\nline2",
			"comma":  "x,y",
			"tab":    "a\tb",
			"binary": "\x00\x01",
			"empty":  "",
		}
		s.TPMSocket = ""
		got, err := Command(s)
		require.NoError(t, err)
		joined := strings.Join(got, " ")
		assert.Contains(t, joined, "-name guest=a,,b")
		assert.Contains(t, joined, "type=1,manufacturer=Acme,, Inc,product=box,version=2,serial=SN1")
		assert.Contains(t, joined, "type=11,value=io.systemd.credential.binary:binary=AAE=")
		assert.Contains(t, joined, "type=11,value=io.systemd.credential:comma=x,,y")
		assert.Contains(t, joined, "type=11,value=io.systemd.credential:empty=")
		assert.Contains(t, joined, "type=11,value=io.systemd.credential.binary:multi=bGluZTEKbGluZTI=")
		assert.Contains(t, joined, "type=11,value=io.systemd.credential.binary:tab=YQli")
		assert.NotContains(t, joined, "tpm")
	})
	t.Run("no netdev no credentials", func(t *testing.T) {
		s := bootSpec()
		s.Netdevs, s.Credentials, s.ExtraDrives = nil, nil, nil
		s.KernelCmdlineExtra = ""
		got, err := Command(s)
		require.NoError(t, err)
		joined := strings.Join(got, " ")
		assert.NotContains(t, joined, "-netdev")
		assert.NotContains(t, joined, "type=11")
		assert.NotContains(t, joined, "-no-reboot")
	})
}

func TestCommandErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(s *Spec)
		want   string
	}{
		{name: "no id", mutate: func(s *Spec) { s.ID = "" }, want: "vm id is required"},
		{name: "bad phase", mutate: func(s *Spec) { s.Phase = "reboot" }, want: `phase must be "install" or "boot"`},
		{name: "no cpus", mutate: func(s *Spec) { s.CPUs = 0 }, want: "cpus must be at least 1"},
		{name: "no memory", mutate: func(s *Spec) { s.MemoryMiB = 0 }, want: "memory must be at least 1 MiB"},
		{name: "install without uki", mutate: func(s *Spec) { s.UKI = "" }, want: "install phase needs a UKI"},
		{name: "install without installer", mutate: func(s *Spec) { s.Installer = "" }, want: "install phase needs the installer DDI"},
		{name: "no target", mutate: func(s *Spec) { s.Target = "" }, want: "target disk is required"},
		{name: "cid too low", mutate: func(s *Spec) { s.VsockCID = 2 }, want: "vsock cid must be in 3..4294967294, got 2"},
		{name: "cid too high", mutate: func(s *Spec) { s.VsockCID = 0xFFFFFFFF }, want: "vsock cid must be in 3..4294967294"},
		{name: "no ovmf", mutate: func(s *Spec) { s.OVMFVars = "" }, want: "OVMF code and vars paths are required"},
		{name: "no serial log", mutate: func(s *Spec) { s.SerialLog = "" }, want: "serial log path is required"},
		{name: "no qmp", mutate: func(s *Spec) { s.QMPSocket = "" }, want: "QMP socket path is required"},
		{name: "drive without path", mutate: func(s *Spec) { s.ExtraDrives = []Drive{{Serial: "d"}} }, want: `drive "d" has no path`},
		{name: "drive serial too long", mutate: func(s *Spec) { s.ExtraDrives = []Drive{{Path: "/x", Serial: strings.Repeat("a", 21)}} }, want: "must be 1-20 characters"},
		{name: "drive serial bad chars", mutate: func(s *Spec) { s.ExtraDrives = []Drive{{Path: "/x", Serial: "a b"}} }, want: "must be 1-20 characters"},
		{name: "drive serial reserved", mutate: func(s *Spec) { s.ExtraDrives = []Drive{{Path: "/x", Serial: "target"}} }, want: `drive serial "target" is used twice`},
		{name: "drive serial duplicate", mutate: func(s *Spec) {
			s.ExtraDrives = []Drive{{Path: "/x", Serial: "d"}, {Path: "/y", Serial: "d"}}
		}, want: `drive serial "d" is used twice`},
		{name: "netdev without id", mutate: func(s *Spec) { s.Netdevs[0].ID = "" }, want: `netdev id "" must be`},
		{name: "netdev duplicate", mutate: func(s *Spec) { s.Netdevs = append(s.Netdevs, s.Netdevs[0]) }, want: `netdev id "net0" is used twice`},
		{name: "netdev backend with id", mutate: func(s *Spec) { s.Netdevs[0].Backend = "user,id=net0" }, want: "carry no id"},
		{name: "netdev empty backend", mutate: func(s *Spec) { s.Netdevs[0].Backend = "" }, want: "backend must be set"},
		{name: "netdev backend injects option", mutate: func(s *Spec) { s.Netdevs[0].Backend = ",addr.path=/x" }, want: "must start with the backend type"},
		{name: "netdev backend spaces", mutate: func(s *Spec) { s.Netdevs[0].Backend = "stream x" }, want: "must start with the backend type"},
		{name: "drive format injects option", mutate: func(s *Spec) { s.ExtraDrives = []Drive{{Path: "/x", Serial: "d", Format: "raw,file=/etc/shadow"}} }, want: `format "raw,file=/etc/shadow" must be raw or qcow2`},
		{name: "qmp socket too long", mutate: func(s *Spec) { s.QMPSocket = "/" + strings.Repeat("q", MaxUnixSocketPath) }, want: "QMP socket path exceeds 107 bytes"},
		{name: "tpm socket too long", mutate: func(s *Spec) { s.TPMSocket = "/" + strings.Repeat("t", MaxUnixSocketPath) }, want: "TPM socket path exceeds 107 bytes"},
		{name: "netdev bad mac", mutate: func(s *Spec) { s.Netdevs[0].MAC = "52:54:00" }, want: "is not a 48-bit address"},
		{name: "netdev 64-bit mac", mutate: func(s *Spec) { s.Netdevs[0].MAC = "01:02:03:04:05:06:07:08" }, want: "is not a 48-bit address"},
		{name: "credential empty name", mutate: func(s *Spec) { s.Credentials[""] = "x" }, want: `credential name "" must be 1-255`},
		{name: "credential dot", mutate: func(s *Spec) { s.Credentials[".."] = "x" }, want: "not . or .."},
		{name: "credential too long", mutate: func(s *Spec) { s.Credentials[strings.Repeat("n", 256)] = "x" }, want: "must be 1-255"},
		{name: "credential slash", mutate: func(s *Spec) { s.Credentials["a/b"] = "x" }, want: "without / : ="},
		{name: "credential equals", mutate: func(s *Spec) { s.Credentials["a=b"] = "x" }, want: "without / : ="},
		{name: "credential colon", mutate: func(s *Spec) { s.Credentials["a:b"] = "x" }, want: "without / : ="},
		{name: "credential non-ascii", mutate: func(s *Spec) { s.Credentials["nämé"] = "x" }, want: "printable ASCII"},
		{name: "cmdline extra multiline", mutate: func(s *Spec) { s.KernelCmdlineExtra = "a\nb" }, want: "single line"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := installSpec()
			tc.mutate(&s)
			_, err := Command(s)
			require.Error(t, err)
			assert.ErrorIs(t, err, apierr.ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestNetdevBackendAcceptsEscapedValues(t *testing.T) {
	s := bootSpec()
	s.Netdevs[0].Backend = "stream,addr.type=unix,addr.path=" + EscapeOption("/run/a,b/qemu.sock") + ",reconnect-ms=500"
	args, err := Command(s)
	require.NoError(t, err)
	assert.Contains(t, args, "stream,addr.type=unix,addr.path=/run/a,,b/qemu.sock,reconnect-ms=500,id=net0")
}
