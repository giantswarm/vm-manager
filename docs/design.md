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
| User-data executor | Go guest agent (`vm-agent`) applies the CAPI cloud-config subset (proposed default) | sysinstall/firstboot cover OS identity, not kubeadm; agent exists anyway for the quote |
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
  - `vm-agent` with `vm-agent-attest.service`, `vm-agent-bootstrap.service`,
    `systemd-report-upload.timer` posting to `.../report`.
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
   ESP, initrd: verity root, repart grows var, systemd-imdsd early network + import
   (hostname, ssh key, user-data -> `/run/credstore`), systemd-firstboot (machine-id,
   hostname, locale/timezone), networkd DHCP from the virtual network.
5. `systemd-sysupdate --component=kubernetes update` pulls the requested version; the IMDS
   directory view lists only the version this VM was created with. `systemd-sysext merge`
   measures it into PCR 13.
6. `vm-agent attest`: fetches a nonce, quotes PCRs 0-7, 11, 13 with an AK, posts quote +
   event logs. vm-manager verifies and marks the VM attested; until then `/user-data` is 403.
7. `vm-agent bootstrap`: reads user-data through `io.systemd.InstanceMetadata`, applies the
   cloud-config subset (`write_files`, `runcmd`, `users`, `hostname`), which runs
   `kubeadm init|join` and writes `/run/cluster-api/bootstrap-success.complete`.
8. PID 1 sends `READY=1` over vsock; `systemd-report upload` pushes metrics on a timer.

Fast path for later: pre-install on the host with `systemd-repart` from the same
`repart.sysinstall.d` definitions against the base DDI (needs root or `io.systemd.Repart`),
skipping phase A when TPM-bound install credentials are not needed.

## IMDS contract (Giant Swarm provider)

Plain HTTP, `GET http://169.254.169.254/giantswarm/v1<key>`, text bodies, 404 for unknown
keys, 403 for gated keys. The Go key table is the single source of truth and generates the
hwdb record at image build time.

| Key | hwdb property | Content |
|---|---|---|
| `/hostname` | `IMDS_KEY_HOSTNAME` | VM name |
| `/region`, `/zone` | `IMDS_KEY_REGION/ZONE` | host name, network name |
| `/public-keys/0` | `IMDS_KEY_SSH_KEY` | first authorized key |
| `/user-data` | `IMDS_KEY_USERDATA` | CAPI bootstrap data, gated by attestation |
| `/instance-id`, `/kubernetes-version`, `/metadata/<k>` | extra | plain values |
| `/attest/nonce`, `/attest/quote` | agent only | attestation protocol below |
| `/report` | agent only | `systemd-report upload` sink |
| `/sysupdate/<component>/` | sysupdate | `SHA256SUMS`, `SHA256SUMS.gpg`, artifacts |

Attestation protocol: `GET /attest/nonce` returns 32 hex bytes valid 5 minutes.
`POST /attest/quote` with JSON `{nonce, ak_pub, ek_pub, quote, signature, pcrs:{sha256:{"0":..}},
event_log, userspace_log}` (binary fields base64). Verification: signature over the quote
with `ak_pub`, nonce match, PCR digest matches `pcrs`, PCR 11 in the set computed with
`systemd-measure calculate` for the image's UKI and phase path
`enter-initrd:leave-initrd:sysinit:ready`, PCRs 0-7 and 13 equal the image policy's golden
values recorded by `vm-manager image golden`. AK is trusted on first use in the prototype
(vm-manager created the VM and its network seconds earlier); EK-certified AKs come later.

## How CAPI fits

CAPI's kubeadm bootstrap provider writes cloud-config (or Ignition) into a Secret and expects
an infrastructure provider to hand it to a VM that runs it and registers as a node. With
vm-manager:

1. cluster-manager (or an agent) calls `create_vm` with `user_data` set to the bootstrap
   Secret's content, plus `kubernetes_version`, network and sizing.
2. The VM installs (phase A), boots (phase B), attests, and only then receives the
   user-data; `vm-agent bootstrap` runs kubeadm exactly as cloud-init would.
3. The node's `providerID` is `giantswarm-vm://<vm-id>`, set via kubelet extra args in the
   KubeadmConfig; `get_vm` reports IP, attestation, and readiness so the caller can set the
   infra Machine ready when the node registers.
4. Control plane endpoint: v1 uses the first control plane VM's IP and a host port-forward
   for 6443; multi control plane later uses kube-vip on a reserved IP of the network.
5. Remediation is delete + recreate. A thin CAPI infrastructure provider (or cluster-manager
   glue) that wraps this API is a follow-up; the prototype's e2e tests use kubeadm
   cloud-config shaped exactly like CAPI's output.

What sysinstall/firstboot do not cover, and therefore needs the agent: running kubeadm,
writing CAPI's files, and proving integrity before secrets are released.

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
| T0 unit | every package with fakes (fake runtime, fake storage, httptest IMDS, CAPI cloud-config fixtures, go-tpm simulator) | < 30 s | every PR |
| T1 contract | in-process MCP client over httptest against the fake runtime; REST parity via `statusFor` | < 10 s | every PR |
| T2 image | mkosi build + `image-verify`: `systemd-dissect`, `ukify inspect`, `systemd-measure calculate` | minutes, cached | every PR touching images/ |
| T3 boot e2e | Go tests tagged `e2e` with the real runtime on KVM: install + reboot + READY, IMDS + firstboot, attestation pass and tampered fail, sysext merged, single-node kubeadm, 1 CP + 1 worker | < 90 s per test | every PR on a KVM runner, nightly full |

Boot-time budgets are asserted in T3 and exported as metrics: installer phase to reboot
< 60 s, installed boot to READY < 10 s.

## Host prerequisites

qemu (present), edk2-ovmf (present), swtpm, mkosi >= 27, erofs-utils, /dev/kvm and
/dev/vhost-vsock accessible (world-writable on the dev host), Go 1.26+.
