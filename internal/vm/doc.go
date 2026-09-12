// Package vm is the VM service: the lifecycle state machine, its persistence
// and the orchestration of storage, swtpm, the virtual network, the two QEMU
// boot phases and the metadata service (docs/design.md "Host architecture",
// "Boot flow"). internal/api exposes the Service methods as MCP tools and
// REST; nothing in here knows about transports.
//
// # States
//
//	State       Meaning                                                    Process
//	creating    id, CID, key, volume and lease being allocated             none
//	installing  installer boot (phase A): sysinstall copies the OS         QEMU -kernel UKI, -no-reboot
//	booting     installed boot (phase B) started, guest not yet ready      QEMU from the target disk
//	attesting   attestation required and the guest asked for a nonce       same
//	ready       guest sent READY=1 over vsock                              same
//	running     process up, READY=1 not received within BootTimeout        same
//	stopping    Stop/Reboot/Delete asked the guest to power down           exiting
//	stopped     no process; the disk is installed and can be started       none
//	failed      no process; LastError says why                             none
//	deleting    Delete in progress                                         none or exiting
//
// Transitions:
//
//	creating    -> installing   Create: resources allocated, installer started
//	creating    -> (gone)       Create failed before the installer was up; nothing is kept
//	installing  -> booting      installer exited 0 (sysinstall rebooted, -no-reboot ended QEMU)
//	installing  -> failed       installer exited non-zero, or InstallTimeout (killed); console tail in LastError
//	installing  -> stopped      installer exited 0 but Close began before phase B started; Start resumes it
//	booting     -> attesting    RequireAttestation and the guest fetched its first nonce
//	booting     -> ready        READY=1
//	attesting   -> ready        READY=1 (user-data was released by a verified initrd quote before)
//	booting|attesting -> running   BootTimeout without READY=1; LastError notes it, the guest keeps running
//	running     -> ready        a late READY=1
//	<live>      -> stopping     Stop, Reboot, Delete
//	stopping    -> stopped      process exited
//	<live>      -> stopped      process exited 0 on its own (guest poweroff)
//	<live>      -> failed       process exited non-zero on its own, or phase B failed to start
//	stopped|failed -> booting   Start (phase B only; a VM that never installed cannot be started)
//	<any>       -> deleting     Delete; the record disappears when it is done
//
// <live> is installing, booting, attesting, ready or running. A reboot from
// inside the guest is invisible here: only phase A runs with -no-reboot, phase
// B lets QEMU reset internally and the next READY=1 keeps the VM ready.
//
// Invariant: once Close or Delete has begun for a VM, no process is started
// for it. Every start (Create, Start, the install-to-boot handoff) holds the
// entry's opMu and checks the record under s.mu; Delete takes opMu and moves
// the record to deleting first, Close sets a service-wide closing flag under
// s.mu and stops each VM under its opMu. A start already holding opMu
// therefore finishes and is stopped by Close or Delete; one that has not
// begun refuses (Create and Start with apierr.ErrConflict, the handoff by
// leaving the installed VM stopped). Close returns within StopTimeout per VM
// plus whatever start was in flight.
//
// # Milestones Create can wait for
//
// Spec.WaitFor blocks Create until installed (InstalledAt set), attested
// (Attestation.UserDataReleased, which is immediate on phase B start when
// attestation is not required) or ready (ReadyAt set). A failure returns the
// record with ErrFailed, an overrun ErrTimeout while the VM continues.
//
// # Attestation
//
// The service implements imds.Resolver (lease IP -> instance snapshot),
// imds.ReportSink (last systemd-report upload, report.json) and wraps the
// configured imds.Attestor so that a verified initrd quote sets
// Attestation.UserDataReleased (imds.ReleasesUserData); the ready-stage quote
// is recorded for get_vm_attestation. Attestation is reset on every installed
// boot, so user-data is gated again after Start. Without RequireAttestation
// user-data is released as soon as phase B starts.
//
// # State directory
//
//	<state>/networks.json          []network.State: specs and leases, restored before VMs
//	<state>/vms/<id>/vm.json       the VM record, written atomically on every transition
//	<state>/vms/<id>/user-data     Ignition JSON, served as /user-data
//	<state>/vms/<id>/ssh_key       vm-manager's per-VM ed25519 key (Exec)
//	<state>/vms/<id>/ssh_host_key  the guest's host key, pinned on first Exec
//	<state>/vms/<id>/ovmf_vars.fd  per-VM copy of the OVMF variable store
//	<state>/vms/<id>/tpm/          swtpm state, shared by both phases
//	<state>/vms/<id>/console.log   serial console
//	<state>/vms/<id>/qmp.sock      QMP socket of the running QEMU
//	<state>/vms/<id>/report.json   last guest report
//
// The target disk is the storage volume vm-<id>. Keep <state> short: unix
// socket paths below it are limited to qemu.MaxUnixSocketPath bytes.
//
// # Startup sequence for the command wiring
//
//  1. storage.Detect(ctx, log, fallbackDir) -> Options.Storage
//  2. network.NewManager(stateDir, log) -> Options.Networks = Networks(m)
//  3. qemu.ListenNotify(port, log) -> Options.Notify
//  4. tpm.New(...) -> Options.TPM = TPM(m); qemu.New(...) -> Options.Runtime = QEMURuntime(r)
//  5. images.Load(dir, log) -> Options.Images
//  6. svc, _ := New(opts); svc.Load(ctx): restores the networks with their
//     leases, starts one IMDS server per network (ServeIMDS) and loads the VM
//     records. A VM recorded with a live process is reattached when its
//     processes still run (see below); otherwise it becomes stopped with a
//     note.
//  7. serve the API; on shutdown svc.Close(ctx) leaves the VMs running for
//     the next vm-manager (DetachOnClose) or stops every one gracefully.
//
// # Restarts
//
// With the systemd launcher (proc.SystemdExec) QEMU and swtpm are transient
// units that outlive vm-manager, and the record carries their handles
// (VM.Processes). Close with DetachOnClose closes the QMP connection and
// leaves them be; Load reattaches in reattach.go: volume, network lease,
// swtpm, QEMU with QMP, notify subscription, supervisor. A process that
// exited while nobody watched is settled through the same supervisor path
// as any exit; a record whose processes are gone, or that came from the
// plain process launcher, becomes stopped with a note. Networks are
// recreated from networks.json, so leases and MACs survive restarts, and
// QEMU reconnects its netdev by itself. The vsock notify port is recorded
// in the state dir (qemu.ListenNotifyPersistent) because the guests carry
// it in a credential from boot; the state dir itself is flock(2)ed
// (lock.go) so no second vm-manager serves it.
//
// Known limitation: swtpm is reattached on the unit's word alone. A swtpm
// whose unit cannot be queried (or is gone) while the process still lives
// is treated as gone: the VM stays tracked and running, its swtpm is
// neither stopped nor stopped later with the VM, and a warning is logged.
package vm
