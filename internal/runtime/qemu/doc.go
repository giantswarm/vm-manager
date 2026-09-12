// Package qemu is the process-level VM runtime: it turns a Spec into the
// qemu-system-x86_64 argument list, starts and supervises the process, and
// gives the caller the three channels back into a running VM: QMP, the
// serial console log and the AF_VSOCK READY notification.
//
// A VM goes through two boot phases (docs/design.md "Boot flow"):
//
//   - PhaseInstall, the installer boot: OVMF boots the UKI directly
//     (-kernel), the base DDI is attached read-only as serial=installer and
//     the blank target volume as serial=target. Credentials reach the guest
//     as SMBIOS type 11 io.systemd.credential strings; among them
//     vm.install-target names the disk and vmm.notify_socket the host's
//     vsock port. systemd-sysinstall copies the OS onto the target and
//     reboots; with -no-reboot the reboot ends the QEMU process, which is how
//     the caller learns the installation finished.
//   - PhaseBoot, every boot afterwards: OVMF -> systemd-boot -> UKI from the
//     target's ESP, no -kernel and no installer disk. The first installed
//     boot adds ignition.firstboot through io.systemd.stub.kernel-cmdline-extra.
//
// Both phases share the machine: q35 with KVM and the host CPU model, a per-VM
// copy of the OVMF variable store, swtpm behind tpm-crb, vhost-vsock with the
// VM's CID, virtio-blk disks identified by serial (so the guest sees
// /dev/disk/by-id/virtio-<serial>), and a virtio-net device on a netdev the
// network package prepares. Command is a pure function of the Spec, Runtime
// adds the process lifecycle, QMP the control channel, NotifyListener the
// READY=1 signal, and the tpm package the swtpm that must run before QEMU.
//
// QEMU is launched through a proc.Exec; with the systemd launcher it runs
// as the transient unit vm-manager-<id>-qemu and outlives vm-manager.
// Instance.Handle is what the caller persists, Runtime.Attach turns it back
// into an Instance after a restart (process, exit status, QMP), and
// Instance.Detach lets go of a VM that is meant to keep running.
package qemu
