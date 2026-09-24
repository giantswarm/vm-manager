# vm-manager

[![test](https://github.com/giantswarm/vm-manager/actions/workflows/test.yml/badge.svg)](https://github.com/giantswarm/vm-manager/actions/workflows/test.yml)
[![image](https://github.com/giantswarm/vm-manager/actions/workflows/image.yml/badge.svg)](https://github.com/giantswarm/vm-manager/actions/workflows/image.yml)
[![e2e](https://github.com/giantswarm/vm-manager/actions/workflows/e2e.yml/badge.svg)](https://github.com/giantswarm/vm-manager/actions/workflows/e2e.yml)
[![chart](https://github.com/giantswarm/vm-manager/actions/workflows/chart.yml/badge.svg)](https://github.com/giantswarm/vm-manager/actions/workflows/chart.yml)
[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/vm-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/vm-manager/tree/main)

VM provisioning service for the Giant Swarm Agent Platform: the write surface for
**virtual machines**, the sibling of [agent-manager](https://github.com/giantswarm/agent-manager)
(agents) and [model-manager](https://github.com/giantswarm/model-manager) (models) next
to the planned cluster-manager, which will hand the VMs it creates to Cluster API. It runs
on a KVM host (a laptop, a bare-metal node, a CI runner) and turns it into a small cloud
region: every VM gets an instance metadata service, a vTPM with measured boot, an
immutable OS image, a Kubernetes node stack as a versioned layer, and attestation before
it receives its bootstrap secrets. On the Agent Platform it runs as a pod of the KVM node.

The whole guest and host flow is built from systemd primitives rather than a
cloud-init-style agent. The image comes from mkosi (Arch Linux, systemd 261, a UKI with
signed expected PCR 11 values, an erofs root under signed dm-verity); the first boot runs
`systemd-sysinstall` onto a volume from the systemd storage provider; identity arrives as
system credentials and through `systemd-imdsd` from the IMDS; user-data is applied by
Ignition in the initrd; Kubernetes is a `systemd-sysext` pulled with `systemd-sysupdate`
and measured into PCR 13; `/etc` persists through an overlay on the var partition;
readiness is `READY=1` over vsock, and the guest reports back with `systemd-report`. The
only Giant Swarm binary in the guest, `vm-agent`, does one thing: quote the TPM. A VM
built this way behaves like an AWS or GCP instance from CAPI's point of view, on hardware
you own, without a hypervisor management plane in between.

The same operations are exposed twice from one process:

- **REST/JSON** under `/api/v1` for the portal backend and scripts. Contract in
  [`api/openapi.yaml`](api/openapi.yaml), also served at `/api/v1/openapi.yaml`.
- **MCP** (streamable HTTP, `/mcp`) for muster and agents: tools `get_info`, `get_host`,
  `list_images`, `get_image`, `list_networks`, `get_network`, `list_vms`, `get_vm`,
  `get_vm_console`, `get_vm_metrics`, `get_vm_attestation`, `create_network`,
  `delete_network`, `create_vm`, `start_vm`, `stop_vm`, `reboot_vm`, `delete_vm`,
  `exec_vm`, `forward_port` (through muster: `x_vm-manager_<tool>`). Every description
  says whether it writes and what to call first; the read-only, destructive, idempotent
  and open-world annotations are set, and the server's instructions tell a model the
  call order (`get_host` first, then images and a network, then `create_vm`).

Both surfaces call the same service and return the same JSON. On the platform vm-manager
is a **pod**, like its siblings: the [Helm chart](helm/vm-manager) runs it privileged on
a KVM node with `/dev/kvm` and `/dev/vhost-vsock` from the node, QEMU and swtpm as its
children, the image directory mounted from a claim or a node path, OAuth against the
platform identity, and registers it with muster through its own `MCPServer` CR carrying
the `agent-platform.giantswarm.io/tool-group: agent-platform` label. The
[agent-platform](https://github.com/giantswarm/agent-platform) meta chart installs it as
`components.vm-manager`; [agentlab](https://github.com/giantswarm/agentlab) runs that
component on a local kind cluster with a build from this checkout and proves the chain
headlessly (`agentlab vm-manager-test`), see
[docs/development.md](docs/development.md#testing-against-the-agent-platform-agentlab).
Design decisions and their reasons are in [docs/design.md](docs/design.md), the plan in
[docs/plan.md](docs/plan.md), the install on a host or a cluster in
[docs/install.md](docs/install.md), development in [docs/development.md](docs/development.md).

## API at a glance

| Operation | REST | MCP tool | Writes |
|---|---|---|---|
| This server's build (release version, commit, build time) and the names of its tools | — | `get_info` | no |
| Host capabilities: kernel, CPUs, memory, `/dev/kvm`, `/dev/vhost-vsock`, qemu / swtpm / systemd versions, OVMF image and its build, storage providers, `ready` + `missing` | `GET /api/v1/host` | `get_host` | no |
| List the bootable images (id, version, UKI, disk, Kubernetes sysext versions, PCR policy) | `GET /api/v1/images` | `list_images` | no |
| Describe one image (`<id>_<version>` or bare id = newest) | `GET /api/v1/images/{ref}` | `get_image` | no |
| List the virtual networks with gateway and leases | `GET /api/v1/networks` | `list_networks` | no |
| Describe one network | `GET /api/v1/networks/{name}` | `get_network` | no |
| List every VM record, oldest first | `GET /api/v1/vms` | `list_vms` | no |
| Fetch one VM: state, IP, attestation, last error | `GET /api/v1/vms/{id}` | `get_vm` | no |
| Tail of the serial console (`lines`, default 100) | `GET /api/v1/vms/{id}/console?lines=` | `get_vm_console` | no |
| Per-VM metrics: `host` (state, attestation, QEMU `cpu_seconds` and `memory_rss_bytes`, `disk_bytes`, `install_seconds`, `boot_to_ready_seconds`, the network's byte counters) and `guest` (summary of the last `systemd-report` upload, `null` until one arrived) | `GET /api/v1/vms/{id}/metrics` | `get_vm_metrics` | no |
| The guest's last `systemd-report` upload in full (REST only: too large for a tool result) | `GET /api/v1/vms/{id}/report` | — | no |
| The current boot's attestation verdicts | `GET /api/v1/vms/{id}/attestation` | `get_vm_attestation` | no |
| Create a network (`name`, `cidr`, `dns_search_domain`); IMDS always on | `POST /api/v1/networks` | `create_network` | yes |
| Delete a network; `conflict` while a VM is attached | `DELETE /api/v1/networks/{name}` | `delete_network` | yes, destructive |
| Create a VM (`name`, `image`, `kubernetes_version`, `cpus` 2, `memory_mib` 2048, `disk_gib` 20, `network` default, `user_data`, `ssh_authorized_keys`, `hostname`, `metadata`, `require_attestation` true, `wait_for` ready) | `POST /api/v1/vms` | `create_vm` | yes |
| Boot a stopped or failed VM from its disk | `POST /api/v1/vms/{id}/start` | `start_vm` | yes |
| Power a VM down (ACPI, then SIGTERM/SIGKILL after `--stop-timeout`) | `POST /api/v1/vms/{id}/stop` | `stop_vm` | yes |
| Stop, then start | `POST /api/v1/vms/{id}/reboot` | `reboot_vm` | yes |
| Stop if running, remove disk, vTPM state, lease, forwards and record | `DELETE /api/v1/vms/{id}` | `delete_vm` | yes, destructive |
| Run a command as root in the guest over ssh (`command` array; non-zero exit is a result) | `POST /api/v1/vms/{id}/exec` | `exec_vm` | yes, destructive |
| Expose a guest TCP port on a host loopback address (`port`) | `POST /api/v1/vms/{id}/forward` | `forward_port` | yes |
| Prometheus exposition, health | `GET /metrics`, `GET /healthz`, `GET /readyz` | — | no |

Request bodies use the tool argument names (snake_case); resources come back as the
service records them (camelCase). Errors are `{"error":{"code","message"}}` with a stable
code, and MCP tool errors carry the same code: `not_found`, `invalid_request`, `conflict`,
`unsupported`, `timeout` (504: a `wait_for` milestone was not reached; the VM keeps
running), `vm_failed` (the VM failed before the milestone; `lastError` and the console say
why), `internal_error`.

## VM lifecycle

A VM needs a network: `create_network`, or the network named `default` that `serve`
creates at startup from `--network-subnet`. `create_vm` allocates a disk on the storage
provider, a vTPM, a lease (MAC and IP are fixed for the VM's life) and a vsock id, then
runs two boots:

| State | What happens |
|---|---|
| `creating` | resources allocated, nothing running yet |
| `installing` | **installer boot**: QEMU boots the image's UKI directly (`-kernel`) with the image attached read-only and the blank target disk; `vm-sysinstall.service` runs `systemd-sysinstall` onto the target and reboots; QEMU runs with `-no-reboot`, its exit ends the phase. About 12 s. |
| `booting` | **installed boot**: OVMF, systemd-boot, the UKI from the target's ESP. Every later boot (`start_vm`, `reboot_vm`) is this phase. |
| `attesting` | `require_attestation` is set and the guest fetched its first nonce |
| `ready` | the guest's service manager sent `READY=1` over vsock: 11 to 15 s after the installed boot starts for the bare image, longer when user-data runs `kubeadm`, which `READY=1` waits for |
| `running` | the process is up but `READY=1` did not arrive within `--boot-timeout`; `lastError` says so, a late `READY=1` still moves it to `ready` |
| `stopping`, `stopped` | `stop_vm`: ACPI power-down, SIGTERM and SIGKILL after `--stop-timeout`; a guest that powers itself off also lands in `stopped`. The disk stays, `start_vm` boots it again. |
| `failed` | the installer exited non-zero or hit `--install-timeout` (the console tail is in `lastError`), or the installed boot's process died; `start_vm` retries an installed disk, `delete_vm` cleans up |
| `deleting` | `delete_vm`: stop if running, remove disk, vTPM state, lease, port forwards and record |

`wait_for` picks how long `create_vm` blocks: `none` returns at once and `get_vm` follows
the progress; `installed`, `attested` or `ready` block for that milestone, bounded by
`--install-timeout` (5 m) and `--boot-timeout` (4 m). Inside the guest, an installed boot
does the following in order:

1. **Firmware and boot loader.** OVMF measures itself, the option ROMs and the SMBIOS
   tables; systemd-boot and the UKI are measured into PCR 4, the UKI's sections and phase
   transitions into PCR 11, the command-line addition and credentials into PCR 12.
2. **Credentials.** The vTPM-sealed credentials `systemd-sysinstall` placed next to the UKI
   (`firstboot.hostname`, `ssh.authorized_keys.root`, `system.machine_id`,
   `vmm.notify_socket`) come back on every boot; SMBIOS type 1 carries
   `manufacturer=GiantSwarm`, which the image's hwdb record keys the IMDS on.
3. **IMDS in the initrd.** `systemd-imds-early-network.service` brings up DHCP on the
   virtual network, `systemd-imds-import.service` turns `/hostname` and `/public-keys/0`
   into credentials.
4. **Persistent state.** The verity root is set up from `roothash=`; `persistent-etc.service`
   mounts the var partition, an overlay for `/etc` with its upper directory on var, and
   `/root` and `/opt` as bind mounts from var, before `initrd-root-fs.target`. The root
   stays the read-only erofs.
5. **Attestation, initrd stage.** `vm-agent attest --stage=initrd` posts a quote of PCRs
   0-7 and 11 (phase path `enter-initrd`) with both event logs. A verified quote releases
   `/user-data`; until then it answers 503.
6. **Ignition** (first installed boot only: vm-manager adds `ignition.firstboot` through the
   stub's `kernel-cmdline-extra` SMBIOS string) fetches `/user-data` and applies CAPI's
   files and units to `/sysroot`; files below `/etc`, `/var`, `/root` and `/opt` land on
   the var partition. A VM without user-data gets 204 and the stages finish with nothing
   to do.
7. **Switch root.** `systemd-firstboot` applies hostname and friends once (the overlay keeps
   them), sshd comes up with `systemd-ssh-generator`'s AF_VSOCK listener and the per-VM key
   `exec_vm` uses, networkd on the virtio NIC.
8. **Kubernetes sysext.** `vm-kubernetes.service` reads `/kubernetes-version` from the IMDS,
   pulls `kubernetes_<kv>.raw` with `systemd-sysupdate --component=kubernetes` (2 s for the
   206 MB, a no-op on later boots), merges it with `systemd-sysext refresh`, extends PCR 13
   with the file's `SHA256SUMS` line, re-applies the extension's modules and sysctls and
   waits for containerd. A VM without a Kubernetes version skips it.
9. **User-data's units.** CAPI's kubeadm unit, ordered `After=vm-kubernetes.service`, runs
   `kubeadm init|join`.
10. **Attestation, ready stage.** `vm-agent attest --stage=ready` quotes PCRs 0-7, 11 and 13
    once the phase path is `enter-initrd:leave-initrd:sysinit:ready`; recorded for
    `get_vm_attestation`.
11. **READY.** PID 1 sends `READY=1` over vsock when the boot transaction is complete, so
    with user-data `ready` means `kubeadm` succeeded. `vm-report-upload.timer` posts
    `systemd-report upload` to the IMDS every 15 s from then on.

Attestation and the user-data gate reset on every installed boot, so `start_vm` and
`reboot_vm` attest again. A reboot from inside the guest is invisible to vm-manager (only
the installer runs with `-no-reboot`). A reboot to ready takes 11 s; what user-data and
firstboot wrote survives it, as do the machine ID and the ssh host key. vm-manager does not
reattach to QEMU across its own restarts: on shutdown every VM is stopped gracefully, on
startup such VMs are `stopped` with a note in `lastError`.

## Images

[`images/`](images/README.md) is a mkosi 27 project that builds, unprivileged, the base
image and the Kubernetes sysext. Everything vm-manager needs from a build is one directory,
the contract of `--image-dir`:

| Path | Content |
|---|---|
| `giantswarm-vm-base_<v>.efi` | the UKI: kernel, initrd (systemd-networkd, systemd-imdsd, `vm-agent`, Ignition), command line with `roothash=`, `.pcrsig` / `.pcrpkey` for PCR 11 |
| `giantswarm-vm-base_<v>.raw` | the disk image: ESP with systemd-boot and the UKI, erofs root, verity hash and signature partitions |
| `policy.json` | `pcr11` per phase path (computed with `systemd-measure calculate` by `make -C images verify`), `pcr13` per Kubernetes version, `golden.sha256` for PCRs 0, 2-4, 6, 7, 13 and `golden_firmware` (the firmware build they belong to) once `vm-manager image golden` ran; a released guest image artifact carries them |
| `sysupdate/base/` | the A/B OS update artifacts the image's `/usr/lib/sysupdate.d/` transfers name, with `SHA256SUMS` and `SHA256SUMS.gpg` |
| `sysupdate/kubernetes/` | `kubernetes_<kv>.raw` per Kubernetes version, `SHA256SUMS`, `SHA256SUMS.gpg` |

The catalog (`internal/images`) pairs `.efi` and `.raw` by stem, reads the Kubernetes
versions from `sysupdate/kubernetes/SHA256SUMS`, and serves both `sysupdate/` trees to the
guests at `http://169.254.169.254/giantswarm/v1/sysupdate/<component>/`, unfiltered: the
manifests are signed at build time, so the guest selects its version explicitly from
`/kubernetes-version` rather than trusting a per-VM listing. `list_images` is what the
catalog found; `create_vm` defaults to the newest version of the newest image and to the
newest Kubernetes version it offers.

Inside the image (`images/mkosi.images/base`): the hwdb record that maps SMBIOS vendor
`GiantSwarm` to the IMDS URL and key names (generated from the Go key table,
`internal/imds`), `repart.sysinstall.d` with the target layout (ESP, root A / verity A /
signature A copied bit for bit, empty B slots for A/B updates, var for the rest of the
disk), the credential-gated `vm-sysinstall.service`, the sysupdate transfers for the OS
and the Kubernetes component, `vm-kubernetes.service`, both `vm-agent-attest.service`
units, the report timer, and the exitrd that lets `systemd-shutdown` unmount var cleanly
under the `/etc` and `/usr` overlays. The Kubernetes sysext
(`images/mkosi.images/kubernetes`) is an erofs `/usr` delta with kubeadm, kubelet, kubectl,
containerd, runc, crictl and the CNI plugins, signed with the verity certificate the base
trusts, its units started through `Upholds=` on `multi-user.target` because a layer merged
during boot cannot be enabled by presets; `/opt/cni/bin` stays a writable bind mount from
var for CNI DaemonSets.

Keys are generated per build (`make -C images keys`, git-ignored `images/keys/`): the
verity certificate, the PCR signing key for the UKI, and the PGP key behind `SHA256SUMS`,
whose public half ships in the image as `/etc/systemd/import-pubring.pgp`. vm-manager only
serves signed artifacts and never holds a private key. PCR 11 values are reproducible from
the UKI; PCR 13 from the sysext's `SHA256SUMS` line; the firmware PCRs are learned once
per image and host firmware, see below.

## Attestation

The guest proves its integrity to vm-manager over the IMDS, twice per boot. `GET
/attest/nonce` returns a 32-byte nonce valid for 5 minutes and one use; `POST /attest/quote`
carries `{stage, nonce, ak_pub, ek_pub, quote, signature, pcrs: {sha256: {...}}, event_log,
userspace_log}`. The verifier (`internal/attest`, `--attestation=verify`, the default)
checks the signature over the quote with the attestation key, the nonce, that the quoted
PCR digest matches the PCR values sent, and then the values:

| PCR | Compared against | Meaning |
|---|---|---|
| 11 | `policy.json` `pcr11[<phase path>]` | the exact UKI (kernel, initrd, command line) and its boot phase |
| 0, 2, 3, 4, 6, 7 | `policy.json` `golden.sha256` | firmware code, option ROMs, boot loader and UKI, Secure Boot policy |
| 13 (ready stage) | `policy.json` `pcr13[<kubernetes version>]`, golden 13 for a VM without one | the exact Kubernetes sysext, or none |
| 1, 5 | recorded, not compared | SMBIOS tables with per-VM credentials, the boot entry, the GPT with per-install UUIDs |

The attestation key is pinned per VM on the first verified initrd quote (trust on first
use: vm-manager created the VM and its vTPM moments earlier); a later quote from a
different key is rejected. Only the initrd stage releases `/user-data`; the ready stage
is recorded for `get_vm_attestation`, which shows both verdicts, the PCRs quoted, whether
user-data was released and, in learn mode, which values were learned. A rejected quote
fails `vm-agent-attest.service` visibly (exit 2, reason on the console, e.g.
`golden mismatch: pcr 0 expected ..., got ...`) but does not stop the boot: with
`require_attestation` (the default) the IMDS keeps answering 503, Ignition's fetch stage
retries for its 2-minute timeout and the boot ends in `emergency.target` without ever
seeing the bootstrap secret; with `require_attestation: false` user-data was released when
the boot started and the verdict is on record only.

Caveats. PCR 1 and 5 cannot be pinned across VMs, because EDK2 measures the SMBIOS tables
(which carry vm-manager's per-VM credential strings) and the boot entry with the ESP's
partition GUID into PCR 1, and PCR 5 holds the installed disk's GPT; both stay in every
quote for forensics. PCR 12, where systemd-stub measures the extra command line
(`ignition.firstboot`) and the credentials, is not quoted or predicted yet, so the policy's
PCR 11 does not cover the command-line addition. The integrity story therefore rests on
PCRs 0, 4, 7, 11 and 13, and on the verity signature and `SHA256SUMS.gpg` for the content
itself.

Released golden values. The release pipeline records the golden values of every
release before it pushes and signs the guest image artifact: the `guest-image` job of
`.circleci/custom.yml` runs `hack/guest-image-golden.sh`, which boots the release's guest
image with what the release's container image measures (its OVMF code and variable
store, its virtio-net option ROM) and its vm-manager binary: a learn-mode boot,
`vm-manager image golden`, then a fresh VM that must verify against the recorded values
with nothing learned. The emulator, which no PCR measures, is Ubuntu 24.04's QEMU 8.2
with the PPA's swtpm in a recorder container, until the recorder moves to the image's own
QEMU 10.2 (whose io_uring main loop stalled the vTPM, #84, and which vm-manager keeps off
io_uring since v0.23.4). A pod of a release therefore verifies both quotes of its first VM without a
learn-mode boot, and a release that moves the firmware ships the values of the new
build.

Learn mode versus golden. A local build's `policy.json` has no golden values and the
verifier rejects every quote. Bring-up of such an image, or of another firmware, is:
start `serve` with `--attestation-learn-golden` (missing golden PCRs are accepted and
recorded on the VM), boot one VM, `vm-manager image golden <image> --from-vm <id>` writes
its verified ready-stage PCRs and the server's firmware build into the image's
`policy.json`, restart without the flag (`hack/guest-image-golden.sh <vm-manager image>
<image dir>`, `make guest-image-golden`, does all of it for a container image). From then
on a boot on other firmware is
rejected; `e2e/attestation_test.go` proves it with a second OVMF build (`golden mismatch`
on PCR 0 and 7, user-data gated, Ignition in its fetch loop). Learn mode is never for
production, it would accept any firmware; `--attestation=noop` (opt-in) verifies nothing
and does not gate user-data.

What invalidates golden values. They belong to one image and one firmware build. A new
image changes PCR 4 (boot loader and UKI), and PCR 13 with its Kubernetes sysext; a new
OVMF build changes PCR 0, which measures the firmware, and PCR 7 when its variable store
template changes. In the container image the firmware is Ubuntu's `ovmf-generic`, pinned
in the `Dockerfile`: a new build arrives as a pull request and release note of its own
(`update OVMF to <version>, re-record golden PCRs`), whose release records the values for
it, and no other release changes it. On a host install it is the host's package, which a
system update replaces. `get_host` reports the build VMs boot with (`firmware`: the
SHA-256 of `ovmfCode`, the dpkg package and version) and `serve` logs it at start, with a
warning per image whose `golden_firmware` names another build. Values recorded under
another build fail every quote with `golden mismatch: pcr 0`, and the verdict says which
build they were recorded for and which one boots; when the build is the recorded one, it
says so, and the boot itself differs.

Recording golden values again. Learn mode accepts only a PCR without a golden value, so
over stale values the learn boot fails with the same mismatch: `vm-manager image golden
<image> --clear` removes them first. In the pod, whose state claim (the chart's
`persistence`) holds the image directory and its `policy.json`, `vm-manager image` reaches
the pod's server and directory without flags:

```sh
kubectl -n <namespace> exec deploy/vm-manager -c vm-manager -- vm-manager image golden <image> --clear
# chart value vm.learnGolden: true; the upgrade restarts the pod, which reads the policy at start
# create one VM with require_attestation: true and wait for ready (create_vm, POST /api/v1/vms), then
kubectl -n <namespace> exec deploy/vm-manager -c vm-manager -- \
  vm-manager image golden <image> --from-vm <id> --token <bearer token, with OAuth on>
# delete the VM and set vm.learnGolden: false: after that restart every boot is compared
```

Without a state claim the pod fetches the guest image at every start and forgets the
values recorded into it; a released artifact brings its own values back each time.

## Networking

Networks are rootless (`internal/network`, gvisor-tap-vsock): each is its own userspace
Ethernet switch with a gVisor TCP/IP stack as gateway, DHCP server, DNS forwarder and NAT,
so vm-manager needs no bridges, tap devices, iptables or `CAP_NET_ADMIN` and runs the same
way on a laptop and a CI runner. For a network `a.b.c.0/24`: `.1` is the gateway, `.2`-`.253`
the pool (MAC and IP derived from each other, so a lease is static for the VM's life and
survives restarts through `networks.json`), `.254` the host alias, `.255` broadcast. VMs on
different networks never see each other, even with overlapping CIDRs; a VM reaches whatever
the host reaches.

The IMDS lives at `169.254.169.254:80` as a virtual IP of the gateway, inside the stack,
exactly where a cloud guest expects it; the guest is identified by its lease IP and
nothing else (no token flow, `X-Forwarded-For` ignored, unknown addresses get 403). The
host alias translates to the host's `127.0.0.1` and is **off by default**
(`EnableHostAlias`): it would expose vm-manager's own API to guests that have not
attested yet. In the other direction `exec_vm` dials into the network for ssh, and
`forward_port` binds a `127.0.0.1:<port>` on the host and proxies it to the guest's port
(6443 for `kubectl`, 22 for ssh); a forward lives until the VM is deleted. Expect hundreds
of Mbit/s, IPv4 only, no UDP or ICMP from the host into the network; a kernel tap/bridge
backend behind the same interface is a listed follow-up.

## Metrics

`GET /metrics` is the Prometheus exposition (`--metrics-enabled`, default on). It is served
outside the OAuth guard like the probes, because scrapers do not run OAuth flows and the
exposition holds no secrets; firewall the path or disable it when that is not acceptable.
Host side, per VM: `vm_manager_vm_state{vm,name,state}` (1/0), `vm_manager_vm_attestation`,
`vm_manager_vm_info`, `vm_manager_vm_created_timestamp_seconds`,
`vm_manager_vm_install_seconds`, `vm_manager_vm_boot_to_ready_seconds` (plus
`_duration_seconds` histograms), `vm_manager_vm_cpu_seconds_total` and
`vm_manager_vm_memory_rss_bytes` of the QEMU process, `vm_manager_vm_disk_bytes`; per
network `vm_manager_network_bytes_total{network,direction}` and
`vm_manager_network_leases`; `vm_manager_vms{state}` and `vm_manager_build_info`.

Guest side, every entry of the guest's last `systemd-report upload` becomes a series:
`io.systemd.Manager.UnitActiveState` for `sshd.service` is
`vm_guest_io_systemd_manager_unit_active_state_info{vm,object="sshd.service",value="active"} 1`,
`io.systemd.Manager.NRestarts` is `vm_guest_io_systemd_manager_nrestarts_total{vm,object}`,
the entry's `fields` are labels. Each upload replaces the previous one; at most
`--metrics-guest-series-limit` series (default 1000) are kept per VM, the rest are counted
in `vm_guest_report_series_dropped_total{vm,reason}`; `vm_guest_report_age_seconds` says how
stale a guest's data is. `get_vm_metrics` is the per-VM summary of both sides;
`GET /api/v1/vms/{id}/report` is the raw upload. `internal/metrics` documents the mapping
and the cardinality policy.

## Identity

The caller, not the host user. With `--enable-oauth` vm-manager is an OAuth 2.1 resource
server ([mcp-oauth](https://github.com/giantswarm/mcp-oauth)) in front of **both** the MCP
endpoint and the REST API; providers `dex` (with `--dex-ca-file` and
`--allow-private-oauth-urls` for a private Dex) and `google`. Nobody logs in to vm-manager
itself: muster forwards the session's IdP id_token byte-identical and the portal sends the
signed-in user's, both validated against the IdP's JWKS because their audience is in
`--oauth-trusted-audiences` (`--sso-allow-private-ips` when the JWKS endpoint is private);
a token for none of the trusted audiences is refused with `401`. Tokens from this server's
own OAuth flow work too, `--allow-public-client-registration` opens dynamic registration
for labs. The caller (`internal/identity`: subject, email, groups, source `sso|oauth`) is on
the request context and in the debug log of every authenticated request; VM records do not
carry it yet.

`/healthz`, `/readyz`, `/metrics` and the OAuth metadata endpoints stay outside the guard.
Without `--enable-oauth` the API is anonymous, which is why the default listener is
`127.0.0.1:8080`: only for a listener nothing but the local user or a trusted proxy can
reach.

## Running

```sh
make build
./vm-manager serve                       # 127.0.0.1:8080, anonymous, verify attestation
./vm-manager serve --listen 0.0.0.0:8080 --enable-oauth --oauth-base-url https://vmm.example \
  --dex-issuer-url https://dex.example/dex --dex-client-id ... --dex-client-secret ... \
  --oauth-trusted-audiences agent-platform
```

Every flag has an environment fallback named in `vm-manager serve --help`; flags win.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `--listen` | `VM_MANAGER_LISTEN` | `127.0.0.1:8080` | listen address |
| `--mcp-path` | `VM_MANAGER_MCP_PATH` | `/mcp` | MCP endpoint path |
| `--state-dir` | `VM_MANAGER_STATE_DIR` | `$XDG_STATE_HOME/vm-manager`, else `~/.local/state/vm-manager`, else `/var/lib/vm-manager` | VM records, consoles, vTPM state, sockets; keep it short (unix socket paths) |
| `--image-dir` | `VM_MANAGER_IMAGE_DIR` | `<state-dir>/images` | the image directory of [Images](#images) |
| `--network-subnet` | `VM_MANAGER_NETWORK_SUBNET` | `192.168.127.0/24` | CIDR of the default network, created at startup when missing |
| `--default-network` | `VM_MANAGER_DEFAULT_NETWORK` | `default` | its name; `create_vm` attaches to it unless told otherwise |
| `--install-timeout` | `VM_MANAGER_INSTALL_TIMEOUT` | `5m` | installer boot ceiling, then `failed` |
| `--boot-timeout` | `VM_MANAGER_BOOT_TIMEOUT` | `4m` | `READY=1` ceiling, then `running`; raise it when user-data runs `kubeadm init` with image pulls |
| `--stop-timeout` | `VM_MANAGER_STOP_TIMEOUT` | `30s` | graceful power-down before SIGKILL, also on shutdown |
| `--ovmf-code`, `--ovmf-vars` | `VM_MANAGER_OVMF_CODE`, `VM_MANAGER_OVMF_VARS` | first pair found: `/usr/share/edk2/x64/OVMF_CODE.4m.fd`, `/usr/share/OVMF/OVMF_CODE_4M.fd`, `/usr/share/OVMF/OVMF_CODE.fd`, `/usr/share/edk2/ovmf/OVMF_CODE.fd` | firmware code image and variable store template (copied per VM) |
| `--notify-port` | `VM_MANAGER_NOTIFY_PORT` | `0` (kernel picks) | vsock port for the guests' `READY=1` |
| `--attestation` | `VM_MANAGER_ATTESTATION` | `verify` | `verify` or `noop` |
| `--attestation-learn-golden` | `VM_MANAGER_ATTESTATION_LEARN_GOLDEN` | `false` | bring-up only, see [Attestation](#attestation) |
| `--metrics-enabled`, `--metrics-guest-series-limit` | `VM_MANAGER_METRICS_ENABLED`, `VM_MANAGER_METRICS_GUEST_SERIES_LIMIT` | `true`, `1000` | see [Metrics](#metrics) |
| `--enable-oauth`, `--oauth-base-url`, `--oauth-provider`, `--oauth-trusted-audiences`, `--sso-allow-private-ips`, `--allow-public-client-registration` | `VM_MANAGER_OAUTH_ENABLED`, `VM_MANAGER_OAUTH_BASE_URL`, `VM_MANAGER_OAUTH_PROVIDER`, `OAUTH_TRUSTED_AUDIENCES`, `SSO_ALLOW_PRIVATE_IPS`, `VM_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION` | off, —, `dex`, —, `false`, `false` | see [Identity](#identity) |
| `--dex-issuer-url`, `--dex-client-id`, `--dex-client-secret`, `--dex-ca-file`, `--allow-private-oauth-urls` | `DEX_ISSUER_URL`, `DEX_CLIENT_ID`, `DEX_CLIENT_SECRET`, `DEX_CA_FILE`, `VM_MANAGER_OAUTH_ALLOW_PRIVATE_URLS` | — | the Dex provider |
| `--google-client-id`, `--google-client-secret` | `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | — | the Google provider |
| `--launcher` | `VM_MANAGER_LAUNCHER` | `auto` | `systemd` (transient services under the user or system manager), `process` (plain children), or `auto` |
| `--detach-vms-on-exit` | `VM_MANAGER_DETACH_VMS_ON_EXIT` | `true` | with the systemd launcher, leave VMs running on shutdown and reattach on the next start |

VMs survive vm-manager restarts. QEMU and swtpm run as transient systemd
services (`vm-manager-<id>-qemu`, `vm-manager-<id>-swtpm`, grouped in
`vm-manager.slice`) under the user's service manager when vm-manager is
unprivileged and under the system manager when it is root; on shutdown the
VMs are left running (`--detach-vms-on-exit`, default on) and the next
`serve` on the same `--state-dir` reattaches to them, exit status included.
The state dir is the contract: `vms/<id>/vm.json` names the units and PIDs,
and the sockets, console and logs below it are where the running processes
expect them, so do not move it while VMs run. vm-manager's own systemd unit
needs no cgroup delegation; without a reachable service manager (`--launcher
process`, or no `$XDG_RUNTIME_DIR/systemd` for an unprivileged run) the VMs
are plain child processes and end with vm-manager, which logs a warning.

The state directory holds `networks.json`, `networks/<name>/qemu.sock`, one `vms/<id>/`
per VM (`vm.json`, `console.log`, `user-data`, `ssh_key` and the pinned `ssh_host_key`,
`ovmf_vars.fd`, `tpm/`, `qmp.sock`, `report.json`), `volumes/` when no systemd storage
provider is present, and by default `images/`. Startup order: storage detection (the
`fs` provider's socket under `/run/systemd/io.systemd.StorageProvider/`, else files),
networks restored with their leases, the vsock notify listener, the image catalog, the
VM records (VMs that were running become `stopped`), then the default network.

Host prerequisites of `create_vm`: KVM (`/dev/kvm`), `vhost_vsock` (`/dev/vhost-vsock`),
`qemu-system-x86_64`, `swtpm` and an OVMF build. systemd (the transient units VMs
survive restarts in) and systemd 261's storage provider (provider volumes instead of
files) are capabilities the report names but does not require: without them VMs are
child processes and volumes plain files — the shape of vm-manager in a pod. No root on a
host: the daemon needs group access to the two devices and nothing else.
`GET /api/v1/host` reports `ready` and `missing` so a client can show what to install. `vm-manager image golden` is the second
command (see [Attestation](#attestation)); `vm-manager version` the third.

As a system service: [`deploy/systemd/vm-manager.service`](deploy/systemd/vm-manager.service)
runs `serve --state-dir=/var/lib/vm-manager --image-dir=/var/lib/vm-manager/images
--listen=127.0.0.1:8080` as the `vm-manager` user
([`sysusers.d/vm-manager.conf`](deploy/systemd/sysusers.d/vm-manager.conf)) with
`SupplementaryGroups=kvm`, `DeviceAllow=` for `/dev/kvm`, `/dev/vhost-vsock` and
`/dev/net/tun`, `StateDirectory=` and `RuntimeDirectory=`, hardening that leaves QEMU,
swtpm and unix sockets working, `EnvironmentFile=-/etc/vm-manager/env` for every other
flag, and `Restart=on-failure`. Stopping the service stops the VMs on it.
[docs/install.md](docs/install.md) walks through packages, device permissions, images,
the first VM, logs and upgrades.

As a pod: the [chart](helm/vm-manager) (the image `gsoci.azurecr.io/giantswarm/vm-manager`
with QEMU, swtpm and Ubuntu's OVMF; the chart in the giantswarm catalog,
`oci://gsoci.azurecr.io/charts/giantswarm/vm-manager`; both released by the generated
CircleCI pipeline on every tag) runs `serve --launcher process` privileged — the runtime
hands a privileged container the node's `/dev/kvm` and `/dev/vhost-vsock`, nothing is
mounted from the node — with the state directory on an emptyDir or a claim
(`persistence`), the guest image fetched into it at pod start by an init container from
the OCI artifact every release publishes (`guestImage`,
`gsoci.azurecr.io/giantswarm/vm-manager-guest-image:<version>`; `vm-manager image pull` /
`image push`), OAuth from the platform's `global.identity`, and the muster `MCPServer` CR
(`muster.mcpServer.enabled`). A pod restart ends the VMs (their records and disks survive
on a claim); the guests' traffic leaves through the pod's own network. The agent-platform
meta chart installs it as `components.vm-manager`, off by default: KVM nodes are not
universal.

## Development

See [docs/development.md](docs/development.md): `make test`, `make lint`,
`make test-integration` (real QEMU, OVMF, swtpm, the storage provider), `make agent`,
`make image` / `make -C images` and `make image-verify`, `make e2e` (seven boot tests on
KVM with the built image, about 6 min, 45 m ceiling), the package layout, the local loop
in learn mode followed by `image golden`, debugging recipes and the "Adding a tool"
checklist.

## Status and roadmap

Prototype. Waves 1 to 3 of [docs/plan.md](docs/plan.md) are on `main` and proven by the
e2e suite on the development host: install 12 s, installed boot to `READY=1` 11-15 s,
reboot to ready 11 s, Kubernetes sysext pull 2 s, a CAPI-shaped control plane Ready 85 s
after `create_vm`, a worker joined 46 s after its own, attestation learn / golden / tamper
in 111 s. Wave 4 is in progress: CI with the image build and a KVM e2e job, these docs,
VMs as transient systemd units so that they survive vm-manager restarts, and
multi-version publishing of the Kubernetes sysext directory.

Follow-ups outside the prototype, in the order they are likely to matter: the CAPI
infrastructure provider or cluster-manager glue that maps Machines to `create_vm`; a
device plugin handing `/dev/kvm` and `/dev/vhost-vsock` to an unprivileged pod; a PCR 12
prediction so the command-line addition is covered by the policy; EK-certified attestation
keys and Secure Boot; a tap/bridge network backend; the host-side pre-install fast path
(`systemd-repart` from the same definitions, skipping the installer boot); a multi
control plane endpoint (kube-vip on a reserved network address).
