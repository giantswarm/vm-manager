# vm-manager design

Status: prototype design, decided in a grill session on 2026-09-12. This document is the
source of truth for architecture decisions; the roadmap and agent plan live in
[plan.md](plan.md).

## What it is

vm-manager is a Go MCP + REST server, a sibling of agent-manager, model-manager and the
planned cluster-manager. It provisions virtual machines on a KVM host that behave like
cloud-provider VMs: an instance metadata service (IMDS), a vTPM with measured boot, an
immutable and composable OS image, fast boot, and Kubernetes prerequisites. Agents on the
Giant Swarm agent platform create VMs through vm-manager and hand them to the CAPI based
cluster-manager to bootstrap workload clusters.

Everything is systemd-native: images are built with mkosi, installed with
systemd-sysinstall onto volumes from the systemd storage provider, configured with
systemd-firstboot and system credentials, fed metadata by systemd-imdsd, layered with
systemd-sysext delivered by systemd-sysupdate, measured with systemd-measure/pcrlock, and
observed through the io.systemd.Metrics Varlink interface and systemd-report.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| VM backend | QEMU/KVM driven directly by vm-manager on a Linux host; runtime behind a Go interface | Full control over SMBIOS, vTPM, vsock, netdev; vmspawn/KubeVirt can be added later |
| Bootstrap contract | Cloud-like IMDS user-data through a Giant Swarm IMDS provider | Exactly how AWS/GCP feed CAPI's kubeadm bootstrap output |
| Base OS | Arch Linux via mkosi (ParticleOS style: UKI, verity, sysupdate) | Host parity (systemd 261), fastest build loop; swap by changing `Distribution=` |
| Provisioning | Installer boot runs systemd-sysinstall onto a storage-provider volume, then reboots into systemd-firstboot | Same flow later works for bare metal; credentials bound to the VM's own vTPM |
| Network | Rootless userspace SDN via gvisor-tap-vsock embedded in vm-manager | No root, runs on laptops and GitHub Actions; IMDS served inside the stack |
| Attestation | swtpm vTPM + measured boot; vm-manager verifies a TPM quote and gates user-data | The "shielded VM" story: only integrity-proven VMs get bootstrap secrets |
| Kubernetes | systemd-sysext layer pulled at boot as a systemd-sysupdate component | One base image, N Kubernetes versions, measured into PCR 13 |
| User-data executor | Ignition in the mkosi initrd; cluster-manager sets KubeadmConfig `format: ignition` | The standard first-boot provisioner for immutable images (Flatcar, FCOS); the guest agent only attests |
| CI | GitHub Actions with KVM for image build + boot e2e; unit/lint everywhere | Hosted runners expose /dev/kvm; no self-hosted infra to start |
| Repo | github.com/giantswarm/vm-manager, public, team-bumblebee | Consistent with siblings |

## Host architecture

```
                      MCP /mcp + REST /api/v1 + /metrics + /healthz
                                      |
                              internal/api, internal/server
                                      |
                                internal/vm (service, state machine, JSON state dir)
      +----------------+---------------+----------------+----------------+
      |                |               |                |                |
 runtime/qemu     network        imds            storage           attest
 (QEMU, swtpm,   (gvisor-tap-  (GiantSwarm      (io.systemd.      (quote verify,
  OVMF, QMP,      vsock L2,     provider,        StorageProvider   expected PCRs,
  vsock READY)    DHCP, NAT,    attest, report,  Varlink client)   user-data gate)
                  169.254.169.254) sysupdate dir)
```

Packages (sibling layering, see `docs/development.md` once it exists):

- `internal/vm`: VM lifecycle state machine `creating -> installing -> booting -> attesting
  -> ready -> running | stopped | failed`, persisted as one JSON file per VM under the state
  dir (`/var/lib/vm-manager` or `$XDG_STATE_HOME/vm-manager`). Each VM runs as a transient
  systemd unit so vm-manager restarts do not kill VMs; QEMU reconnects its netdev with
  `reconnect-ms`.
- `internal/runtime/qemu`: builds the QEMU command for phase A (installer) and phase B
  (installed), manages the per-VM swtpm process and OVMF vars copy, talks QMP
  (status, powerdown, events), captures the serial console to a file, listens on AF_VSOCK
  for `READY=1` from the guest (`vmm.notify_socket` credential).
- `internal/network`: one gvisor-tap-vsock `VirtualNetwork` per network (subnet, gateway,
  DHCP static leases keyed by the VM MAC, DNS, NAT to host, `GatewayVirtualIPs` including
  169.254.169.254). QEMU attaches with `-netdev stream,addr.type=unix`. `Listen` serves IMDS
  inside the stack, `Dial` reaches guests (ssh, kube-apiserver), port-forwards expose 6443.
- `internal/imds`: HTTP handler for the Giant Swarm provider, VM identified by source IP.
- `internal/storage` + `internal/varlink`: minimal JSON-over-AF_UNIX Varlink client;
  `io.systemd.StorageProvider.Acquire` on the `fs` provider creates per-VM target volumes
  (`create=new,size=`), attached as virtio-blk with `serial=target`.
- `internal/attest`: quote verification with go-tpm, expected PCR policy per image.
- `internal/images`: catalog of base DDIs, UKIs, PCR policies and Kubernetes sysext versions.
- `internal/metrics`: host stats per VM plus the last guest `systemd-report` upload, exposed
  as Prometheus families and through `get_vm_metrics`.
- `cmd/vm-agent`: static guest binary, see below.

## Guest image (`images/`)

mkosi project with two images:

- `images/base`: Arch, systemd 261, `Bootable=yes` UKI signed for PCR 11 (`ukify` with the
  build's PCR key), erofs root with `Verity=data/hash/signature`, ESP. Ships:
  - `/usr/lib/udev/hwdb.d/45-imds-giantswarm.hwdb` matching `dmi:*:svnGiantSwarm:*` with
    `IMDS_DATA_URL=http://169.254.169.254/giantswarm/v1` and the key table below; kernel
    cmdline carries `systemd.imds.import=yes` so credentials are imported outside the initrd too.
  - `/usr/lib/repart.sysinstall.d/`: esp, root A/root-verity A/root-verity-sig A with
    `CopyBlocks=auto`, empty B slots (`Label=_empty`) for A/B updates, var.
  - `/usr/lib/repart.d/`: first-boot growth of var.
  - `vm-sysinstall.service`: `ConditionCredential=vm.install-target`, runs
    `systemd-sysinstall $TARGET --erase=yes --confirm=no --welcome=no --chrome=no
    --variables=yes --reboot=yes --kernel=<UKI on the installer ESP>` and forwards
    `firstboot.hostname`, `ssh.authorized_keys.root` and any `firstboot.*` credential it
    finds in `$CREDENTIALS_DIRECTORY` with `--load-credential=`. The stock interactive
    `systemd-sysinstall.service` stays disabled.
  - sshd with `systemd-ssh-generator` (AF_VSOCK port 22) and networkd DHCP.
  - `/usr/lib/sysupdate.kubernetes.d/*.transfer`: url-file source
    `http://169.254.169.254/giantswarm/v1/sysupdate/kubernetes` into
    `/var/lib/extensions/kubernetes_@v.raw`; `/usr/lib/sysupdate.d/` for OS A/B updates from
    `.../sysupdate/base`; `/etc/systemd/import-pubring.pgp` holds the build key.
  - `vm-agent` (attestation only) with `vm-agent-attest.service` in the initrd and the
    real system, and `systemd-report-upload.timer` posting to `.../report`.
  - The initrd (mkosi sub-image) contains systemd-networkd, systemd-imdsd, `vm-agent` and
    Ignition with its stock `ignition-*` units (fetch, disks, mount, files, complete).
    The UKI cmdline carries `ignition.platform.id=metal
    ignition.config.url=http://169.254.169.254/giantswarm/v1/user-data`; vm-manager adds
    `ignition.firstboot` through the `io.systemd.stub.kernel-cmdline-extra` SMBIOS string
    only on the first installed boot. Ignition comes from a pinned upstream release built
    during the image build; Arch has no official package.
- `images/kubernetes`: sysext DDI with kubeadm, kubelet, containerd, runc, crictl,
  cni-plugins and their units; `extension-release.kubernetes` matching the base.

Build outputs per version: `base_<v>.raw`, `base_<v>.efi`, `base_<v>.root.raw` +
`.verity.raw` (split for sysupdate), `kubernetes_<kv>.raw`, `SHA256SUMS`, `SHA256SUMS.gpg`,
`policy.json` (expected PCRs). Signing keys (PGP for SHA256SUMS, PCR key for the UKI) are
generated per build in CI and kept in a git-ignored local dir for developers; vm-manager
only serves signed artifacts and never holds private keys.

## Boot flow

Phase A, installer boot (no persistent disk yet):

1. `create_vm` acquires a target volume, creates the swtpm state dir and OVMF vars copy,
   allocates a MAC + DHCP lease, stores user-data.
2. QEMU starts with `-kernel base_<v>.efi` (direct UKI boot under OVMF), the base DDI
   attached read-only (`serial=installer`), the blank target (`serial=target`), swtpm via
   `tpm-crb`, `-smbios type=1,manufacturer=GiantSwarm,product=vm-manager,serial=<vm-id>`,
   and SMBIOS type 11 credentials: `vm.install-target=/dev/disk/by-id/virtio-target`,
   `firstboot.hostname`, `ssh.authorized_keys.root`, `system.machine_id`,
   `vmm.notify_socket=vsock:2:<port>`.
3. `vm-sysinstall.service` installs the booted OS onto the target: repart copies the
   root/verity/sig partitions bit-identically, creates ESP + B slots + var, `bootctl link`
   installs the UKI plus TPM-encrypted credential files, `bootctl install` adds systemd-boot,
   then reboots. QEMU runs with `-no-reboot`; vm-manager sees the exit and starts phase B.

Phase B, installed boot (every boot from now on):

4. OVMF -> systemd-boot -> UKI (measured into PCR 11 with `.pcrsig`), credentials from the
   ESP. In the initrd: verity root, repart grows var, systemd-imdsd early network brings up
   DHCP from the virtual network, imds import (hostname, ssh key -> `/run/credstore`).
5. Still in the initrd, `vm-agent attest --stage=initrd` fetches a nonce, quotes PCRs 0-7
   and 11 (phase `enter-initrd`) with an AK and posts quote + event logs. vm-manager
   verifies and marks the VM attested; until then `/user-data` is 403.
6. Ignition (first boot only) fetches the Ignition config from `/user-data`, runs its
   disks/files stages, which write CAPI's files and systemd units (kubeadm config, the
   kubeadm unit), then the initrd switches root.
7. Real system: systemd-firstboot (machine-id, hostname, locale/timezone), networkd,
   the guest reads `/kubernetes-version` from IMDS and runs
   `systemd-sysupdate --component=kubernetes update <version>` (explicit version selection;
   the artifact directory is served unfiltered because its `SHA256SUMS` is signed at build
   time), `systemd-sysext merge` activates it (see the PCR 13 note under open points), then CAPI's unit runs `kubeadm init|join` and writes
   `/run/cluster-api/bootstrap-success.complete`. `vm-agent attest --stage=ready` posts a
   second quote covering PCR 13 and the full phase path for `get_vm_attestation`.
8. PID 1 sends `READY=1` over vsock; `systemd-report upload` pushes metrics on a timer.

Fast path for later: pre-install on the host with `systemd-repart` from the same
`repart.sysinstall.d` definitions against the base DDI (needs root or `io.systemd.Repart`),
skipping phase A when TPM-bound install credentials are not needed.

### Open points found while building waves 1 and 2

- **PCR 13 and sysexts.** systemd 261 measures only stub-loaded extensions
  (`<uki>.efi.extra.d/*.sysext.raw` on the ESP) into PCR 13; extensions merged from
  `/var/lib/extensions` are not measured. To keep the Kubernetes layer attested, either the
  phase-A installer pulls the sysext onto the target ESP next to the UKI (a sysupdate transfer
  with `PathRelativeTo=esp`) so the stub loads and measures it on the first installed boot, or
  the ready-stage agent extends PCR 13 with the sysext root hash through `systemd-pcrextend`.
  Decided in wave 3 with the attestation work.
- **Volatile `/etc`.** The first image boots with `systemd.volatile=overlay` because
  firstboot needs a writable `/etc`. Everything Ignition writes there is lost on reboot, so
  Kubernetes nodes would not survive a restart. Wave 3 makes `/etc` persistent (an overlay
  whose upper directory lives in `/var`, set up in the initrd) before the Kubernetes e2e.
- **Host loopback alias.** The virtual network can translate `HostIP()` to the host's
  `127.0.0.1`, which would expose vm-manager's own API to unattested guests. It is opt-in
  per network (`EnableHostAlias`) and off for VM networks.
- **Sysext file lifecycle.** systemd-sysext merges every `kubernetes_*.raw` it finds, so a
  version change must remove the superseded file (the transfer uses `InstancesMax=2`, so
  vm-manager or the guest unit deletes the old one after a successful switch).
- **VM processes and vm-manager restarts.** v1 runs QEMU and swtpm as child processes;
  a vm-manager restart marks VMs stopped. Running each VM as a transient systemd unit
  (`systemd-run --scope`) is the planned fix.

## IMDS contract (Giant Swarm provider)

Plain HTTP, `GET http://169.254.169.254/giantswarm/v1<key>`, text bodies, 404 with an
empty body for unknown keys (systemd-imdsd aborts an error response that carries body
bytes before it reaches its own 404 handling; only a bodyless 404 becomes
`KeyNotFound`, which `systemd-imds --import` tolerates for an absent `/user-data`), 403
for gated keys. The Go key table is the single source of truth and generates the hwdb
record at image build time.

| Key | hwdb property | Content |
|---|---|---|
| `/hostname` | `IMDS_KEY_HOSTNAME` | VM name |
| `/region`, `/zone` | `IMDS_KEY_REGION/ZONE` | host name, network name |
| `/public-keys/0` | `IMDS_KEY_SSH_KEY` | first authorized key |
| `/user-data` | `IMDS_KEY_USERDATA` | CAPI bootstrap data as Ignition JSON, gated by attestation |
| `/instance-id`, `/kubernetes-version`, `/metadata/<k>` | extra | plain values |
| `/attest/nonce`, `/attest/quote` | agent only | attestation protocol below |
| `/report` | agent only | `systemd-report upload` sink |
| `/sysupdate/<component>/` | sysupdate | `SHA256SUMS`, `SHA256SUMS.gpg`, artifacts; served unfiltered, the guest picks the version from `/kubernetes-version` |

Attestation protocol: `GET /attest/nonce` returns 32 hex bytes valid 5 minutes.
`POST /attest/quote` with JSON `{stage, nonce, ak_pub, ek_pub, quote, signature,
pcrs:{sha256:{"0":..}}, event_log, userspace_log}` (binary fields base64, `stage` is
`initrd` or `ready`). Verification: signature over the quote with `ak_pub`, nonce match,
PCR digest matches `pcrs`, PCR 11 equals the value computed with `systemd-measure calculate`
for the image's UKI and the stage's phase path (`enter-initrd` for `initrd`,
`enter-initrd:leave-initrd:sysinit:ready` for `ready`), PCRs 0-7 (and 13 for `ready`)
equal the image policy's golden values recorded by `vm-manager image golden`. Only the
`initrd` stage unlocks `/user-data`; the `ready` stage is recorded for `get_vm_attestation`. AK is trusted on first use in the prototype
(vm-manager created the VM and its network seconds earlier); EK-certified AKs come later.

## How CAPI fits

CAPI's kubeadm bootstrap provider writes cloud-config (or Ignition) into a Secret and expects
an infrastructure provider to hand it to a VM that runs it and registers as a node. With
vm-manager:

1. cluster-manager (or an agent) sets KubeadmConfig `format: ignition` and calls
   `create_vm` with `user_data` set to the bootstrap Secret's Ignition JSON, plus
   `kubernetes_version`, network and sizing. The config should order the kubeadm unit after
   `systemd-sysext.service`; cluster-manager can add that through `additionalConfig`.
2. The VM installs (phase A), boots (phase B), attests from the initrd, and only then does
   Ignition receive the user-data and write CAPI's files and units; kubeadm runs from those.
3. The node's `providerID` is `giantswarm-vm://<vm-id>`, set via kubelet extra args in the
   KubeadmConfig; `get_vm` reports IP, attestation, and readiness so the caller can set the
   infra Machine ready when the node registers.
4. Control plane endpoint: v1 uses the first control plane VM's IP and a host port-forward
   for 6443; multi control plane later uses kube-vip on a reserved IP of the network.
5. Remediation is delete + recreate. A thin CAPI infrastructure provider (or cluster-manager
   glue) that wraps this API is a follow-up; the prototype's e2e tests use kubeadm
   cloud-config shaped exactly like CAPI's output.

What sysinstall/firstboot do not cover: writing CAPI's files and units (Ignition) and
proving integrity before secrets are released (`vm-agent attest`).

## MCP tool surface (v1)

Read-only: `get_host`, `list_images`, `get_image`, `list_networks`, `get_network`,
`list_vms`, `get_vm`, `get_vm_console`, `get_vm_metrics`, `get_vm_attestation`.
Writes: `create_network`, `delete_network`, `create_vm` (name, image, kubernetes_version,
cpus, memory_mib, disk_gib, network, user_data, ssh_authorized_keys, hostname, metadata,
require_attestation, wait_for none|installed|attested|ready), `start_vm`, `stop_vm`,
`reboot_vm`, `delete_vm`, `exec_vm`, `forward_port`. REST mirrors each under `/api/v1`.

## Testing strategy

| Tier | Target | Budget | Runs |
|---|---|---|---|
| T0 unit | every package with fakes (fake runtime, fake storage, httptest IMDS, CAPI Ignition fixtures, go-tpm simulator) | < 30 s | every PR |
| T1 contract | in-process MCP client over httptest against the fake runtime; REST parity via `statusFor` | < 10 s | every PR |
| T2 image | mkosi build + `image-verify`: `systemd-dissect`, `ukify inspect`, `systemd-measure calculate` | minutes, cached | every PR touching images/ |
| T3 boot e2e | Go tests tagged `e2e` with the real runtime on KVM: install + reboot + READY, IMDS + firstboot, attestation pass and tampered fail, Ignition files + units applied, sysext merged, single-node kubeadm, 1 CP + 1 worker | < 90 s per test | every PR on a KVM runner, nightly full |

Boot-time budgets are asserted in T3 and exported as metrics: installer phase to reboot
< 60 s, installed boot to READY < 10 s.

## Host prerequisites

qemu (present), edk2-ovmf (present), swtpm, mkosi >= 27, erofs-utils, /dev/kvm and
/dev/vhost-vsock accessible (world-writable on the dev host), Go 1.26+.
