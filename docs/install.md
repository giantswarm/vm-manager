# Installing vm-manager

vm-manager runs where the KVM devices are: as a pod of a KVM node on the Agent Platform
([On Kubernetes](#on-kubernetes-the-chart) below), or directly on a host — a laptop, a
bare-metal node, a GitHub Actions runner. The host part of this guide installs it as a system service with
[`deploy/systemd/vm-manager.service`](../deploy/systemd/vm-manager.service). The daemon
needs no root: everything it does (QEMU, swtpm, the userspace network, the IMDS) runs
as an unprivileged user with access to three device nodes. What the service is and how
it works is in the [README](../README.md); local development loops are in
[development.md](development.md).

## Host prerequisites

| Need | Arch Linux | Notes |
|---|---|---|
| KVM | kernel module `kvm_intel` / `kvm_amd`, `/dev/kvm` | `get_host` reports `/dev/kvm` under `missing` when it is not accessible |
| vsock | module `vhost_vsock`, `/dev/vhost-vsock` | carries `READY=1` and ssh from the guests; see the udev rule below |
| QEMU | `qemu-system-x86` (or `qemu-full`) | `qemu-system-x86_64` on `$PATH` |
| swtpm | `swtpm` | one vTPM process per VM |
| OVMF | `edk2-ovmf` | searched at `/usr/share/edk2/x64/OVMF_CODE.4m.fd`, `/usr/share/OVMF/OVMF_CODE_4M.fd`, `/usr/share/OVMF/OVMF_CODE.fd`, `/usr/share/edk2/ovmf/OVMF_CODE.fd`; other paths through `--ovmf-code` / `--ovmf-vars` |
| systemd (optional) | `systemd` (any recent version) | transient units the VMs survive restarts in (`--launcher systemd`); without it VMs are child processes of `serve` |
| storage provider (optional) | systemd 261 with `io.systemd.StorageProvider` | when the `fs` provider's socket answers under `/run/systemd/io.systemd.StorageProvider/`, VM disks are provider volumes; otherwise plain files below `<state-dir>/volumes/` |
| ssh client | `openssh` | `exec_vm` and the e2e tests |
| image build (optional) | `mkosi` >= 27, `erofs-utils`, `python-pefile`, `gnupg`, `go` 1.26+, `sfdisk` (util-linux) | only on the machine that builds images, see below |

Any distribution with these packages works; the paths in the table are what the code
probes. `vm-manager serve` starts on a host that lacks some of them and reports the gaps
through `GET /api/v1/host` (`ready: false`, `missing: [...]` — the two devices, the two
binaries and the firmware; systemd and the storage provider are reported as found or not
but never required) so an operator or an agent can see what to install; `create_vm` fails
until `ready` is true.

Device access. `/dev/kvm` is `root:kvm 0660` on every mainstream distribution.
`/dev/vhost-vsock` is often `root:root 0600`; give it to the same group:

```sh
cat >| /etc/udev/rules.d/60-vhost-vsock.rules <<'EOF'
KERNEL=="vhost-vsock", GROUP="kvm", MODE="0660"
EOF
udevadm control --reload && udevadm trigger --name-match=vhost-vsock
echo vhost_vsock >| /etc/modules-load.d/vhost-vsock.conf && modprobe vhost_vsock
```

The service unit adds the `vm-manager` user to `kvm` with `SupplementaryGroups=` and
whitelists exactly `/dev/kvm`, `/dev/vhost-vsock` and `/dev/net/tun` with `DeviceAllow=`.
Nothing else on the host is touched: the virtual networks are userspace
(gvisor-tap-vsock), so no bridges, tap devices, iptables rules or `CAP_NET_ADMIN`.

## Images

vm-manager boots the image built in [`images/`](../images/README.md): the
`giantswarm-vm-base` disk image plus UKI, and the Kubernetes sysext. On a machine with
mkosi:

```sh
git clone https://github.com/giantswarm/vm-manager && cd vm-manager
make -C images                     # keys, base image, kubernetes sysext, verify
```

That leaves in `images/build/`:

```text
giantswarm-vm-base_<v>.efi         UKI (installer boot and the kernel of every boot)
giantswarm-vm-base_<v>.raw         disk image, attached read-only to the installer boot
policy.json                        expected PCR 11 per phase, PCR 13 per Kubernetes version
sysupdate/base/                    A/B OS update artifacts + SHA256SUMS(.gpg)
sysupdate/kubernetes/              kubernetes_<kv>.raw + SHA256SUMS(.gpg)
```

Copy these five entries into the image directory of the host (the default is
`<state-dir>/images`, `/var/lib/vm-manager/images` with the unit below). The catalog
scans for `<id>_<version>.efi` + `.raw` pairs, takes `policy.json` for the image it
names, and serves `sysupdate/` to the guests over the IMDS. A production host gets the
artifacts from wherever your release pipeline publishes them; the directory contract is
the same.

Keys. `make -C images keys` generates development keys into the git-ignored
`images/keys/` (verity certificate, PCR signing key, PGP key for `SHA256SUMS`). They are
baked into the image (`/usr/lib/verity.d/`, `/etc/systemd/import-pubring.pgp`), so the
sysext and the update artifacts of one build only verify against the base image of the
same key set. For production, build with keys from your CA and keep `images/keys/` out
of the host: vm-manager only serves signed artifacts and never holds a private key.

Golden PCR values. A freshly built `policy.json` has PCR 11 and PCR 13 but no golden
values for the firmware PCRs (0, 2-4, 6, 7), and the default `--attestation=verify`
rejects every quote until they exist. They are recorded once per image and host firmware
with `vm-manager image golden`, see [First VM](#first-vm). Copy the resulting
`policy.json` alongside the image to every host with the same OVMF build, or repeat the
step per host.

## The binary and the unit

```sh
make build                                                   # ./vm-manager, static
install -Dm0755 vm-manager /usr/local/bin/vm-manager
install -Dm0644 deploy/systemd/sysusers.d/vm-manager.conf /usr/lib/sysusers.d/vm-manager.conf
install -Dm0644 deploy/systemd/vm-manager.service /etc/systemd/system/vm-manager.service
systemd-sysusers                                             # creates the vm-manager user (in group kvm)
loginctl enable-linger vm-manager                            # its own service manager runs the VMs, see below
install -d -o vm-manager -g vm-manager -m 0750 /var/lib/vm-manager /var/lib/vm-manager/images
cp -r images/build/giantswarm-vm-base_* images/build/policy.json images/build/sysupdate /var/lib/vm-manager/images/
chown -R vm-manager:vm-manager /var/lib/vm-manager/images
systemctl daemon-reload
systemctl enable --now vm-manager
```

The unit runs `vm-manager serve --state-dir=/var/lib/vm-manager
--image-dir=/var/lib/vm-manager/images --listen=127.0.0.1:8080`. Everything else comes
from `/etc/vm-manager/env` (`EnvironmentFile=-`, optional): every flag of `serve` has
the environment variable named in `vm-manager serve --help`, for example

```sh
# /etc/vm-manager/env
VM_MANAGER_NETWORK_SUBNET=192.168.127.0/24
VM_MANAGER_BOOT_TIMEOUT=6m
VM_MANAGER_ATTESTATION_LEARN_GOLDEN=true      # bring-up only, remove after `image golden`
# VM_MANAGER_OAUTH_ENABLED=true and the DEX_* / OAUTH_TRUSTED_AUDIENCES variables, see README "Identity"
```

Flags on the command line win over the environment, so `--listen`, `--state-dir` and
`--image-dir` are fixed by the unit; use a drop-in (`systemctl edit vm-manager`) to change
them. The listener stays on loopback because the API is anonymous without
`--enable-oauth`; put a reverse proxy or the OAuth guard in front before exposing it.

### VMs survive restarts of the service

QEMU and swtpm do not run inside `vm-manager.service`. vm-manager starts them as transient
systemd units (`vm-manager-<id>-qemu.service`, `vm-manager-<id>-swtpm.service`, grouped in
`vm-manager.slice`) of the **vm-manager user's own service manager**, `user@<uid>.service`.
Stopping or restarting `vm-manager.service` leaves them running (`--detach-vms-on-exit`,
default on); the next start reattaches through `vms/<id>/vm.json` in the state dir, exit
status included, and the VMs stay `ready`. Package upgrades therefore need no maintenance
window.

That user manager exists only while the user has a session or lingering is enabled, hence
the `loginctl enable-linger vm-manager` above (it survives reboots; `loginctl show-user
vm-manager -p Linger` says `yes`). vm-manager finds the manager at `/run/user/<uid>`
by itself: `Environment=XDG_RUNTIME_DIR=` in the unit is not needed and `%U` would not
help, since in a system unit `%U` is the UID of the *service manager*, 0, not of `User=`
(`man systemd.unit`, "Specifiers"). The kvm group membership from `sysusers.d` matters here
too: the VMs run outside the unit's `SupplementaryGroups=` and `DeviceAllow=`, so they open
`/dev/kvm` and `/dev/vhost-vsock` with the user's own groups.

Without lingering (or with `--launcher process`) vm-manager falls back to plain child
processes and logs `VMs run as child processes and end with vm-manager` at startup; then
a stop or restart powers every VM down (ACPI, then SIGTERM and SIGKILL after
`--stop-timeout`, 30 s by default; the unit gives the shutdown 90 s) and the VMs come back
`stopped` with a note in `lastError`; `start_vm` boots them again. The startup log line
`vm launcher selected launcher=systemd manager=user` confirms the intended mode, and
`systemd-cgls --user-unit vm-manager.slice` (as the vm-manager user) or `systemctl
--machine vm-manager@ --user list-units 'vm-manager-*'` lists the running VMs.

Two things in the state dir are part of this contract: `notify-port`, the vsock port the
guests report `READY=1` to (fixed in each guest's credentials at boot, so every start
reuses it; a second vm-manager on the same state dir fails with "notify port in use"), and
`lock`, an `flock(2)` that makes a second vm-manager on the same state dir fail fast.

## First run

```sh
journalctl -u vm-manager -f                  # "vm-manager starting", storage provider, image catalog, default network
curl -s localhost:8080/healthz               # ok
curl -s localhost:8080/api/v1/host | jq '{ready, missing, qemu, swtpm, ovmfCode, storageProviders}'
curl -s localhost:8080/api/v1/images | jq '.[] | {id, version, kubernetesVersions}'
curl -s localhost:8080/api/v1/networks | jq '.[] | {name, cidr: .spec.cidr, gateway}'
```

The log names the storage provider it selected (`systemd` or `file`), the number of
images in the catalog (a warning when zero) and the default network: `serve` creates the
network named `default` from `--network-subnet` (`192.168.127.0/24`) when the state
directory does not hold one yet, and restores it with its leases afterwards. To change the
default subnet delete the network (`DELETE /api/v1/networks/default`, once no VM is
attached) and restart; a different subnet in the flag is only logged otherwise. Another
network is one call:

```sh
curl -s -X POST localhost:8080/api/v1/networks \
  -d '{"name":"lab","cidr":"192.168.130.0/24","dns_search_domain":"lab.internal"}' | jq .gateway
```

Every network has its own L2 segment, DHCP, DNS forwarder, NAT to whatever the host
reaches, and the IMDS at `169.254.169.254`.

## First VM

Bring-up of a new image or firmware runs in learn mode once (`env` above or
`--attestation-learn-golden`), records the golden values, then verifies:

```sh
API=localhost:8080/api/v1
ID=$(curl -s -X POST $API/vms -d '{"name":"node-1","wait_for":"ready"}' | jq -r .id)
curl -s $API/vms/$ID | jq '{state, ip, attestation, lastError}'
curl -s $API/vms/$ID/attestation | jq .          # initrd and ready quote: verified, learned PCRs
sudo -u vm-manager vm-manager image golden giantswarm-vm-base --from-vm $ID \
  --server http://127.0.0.1:8080 --image-dir /var/lib/vm-manager/images
# remove VM_MANAGER_ATTESTATION_LEARN_GOLDEN from /etc/vm-manager/env, then
systemctl restart vm-manager
```

`create_vm` with `wait_for: ready` blocks through the installer boot (`installing`, about
12 s), the installed boot (`booting`, `attesting`) and returns once the guest's service
manager sent `READY=1` (11 to 15 s after the installed boot starts). From now on every
VM of that image attests against the recorded values: a boot on other firmware is
rejected with a `golden mismatch` on the PCRs that differ and never receives its
user-data. `image golden` reads the VM's verified ready-stage quote from the running
server and writes `golden.sha256` into the image's `policy.json`, so it needs write
access to the image directory (hence `sudo -u vm-manager`).

Work with the VM:

```sh
curl -s -X POST $API/vms/$ID/exec -d '{"command":["systemctl","is-system-running"]}' | jq .
curl -s -X POST $API/vms/$ID/forward -d '{"port":22}' | jq .           # 127.0.0.1:<port> on the host
curl -s "$API/vms/$ID/console?lines=40" | jq -r .console
curl -s $API/vms/$ID/metrics | jq .host
curl -s -X DELETE $API/vms/$ID -o /dev/null -w '%{http_code}\n'
```

A VM meant for a Kubernetes cluster gets `kubernetes_version` (the sysext version the
guest pulls at boot; default: the newest the image offers) and `user_data`, the CAPI
bootstrap Secret as Ignition JSON, released to the guest only after a verified initrd
quote. The [README](../README.md) "VM lifecycle" and "Attestation" sections describe the
boot; `e2e/kubernetes_cluster_test.go` is a complete control plane plus worker through
the API.

The same operations as MCP tools: point an MCP client at `http://127.0.0.1:8080/mcp`
(streamable HTTP); `development.md` shows the raw `initialize` / `tools/call` exchange
with curl.

## Logs and consoles

| What | Where |
|---|---|
| vm-manager log (slog text; debug logging is the `-v` flag, added through a drop-in on `ExecStart=`) | `journalctl -u vm-manager` |
| serial console of a VM | `GET /api/v1/vms/{id}/console?lines=` (`get_vm_console`), the file `/var/lib/vm-manager/vms/<id>/console.log` |
| why a VM failed | `lastError` of `get_vm` (the console tail for installer failures), then the console |
| the guest journal | `exec_vm` with `["journalctl","-b","-u","vm-agent-attest.service"]`, `["journalctl","-b","-p","warning"]`, `["systemctl","--failed"]` |
| attestation verdicts | `GET /api/v1/vms/{id}/attestation` (`get_vm_attestation`); rejected quotes are also on the guest console (`vm-agent-attest.service`) |
| the guest's last `systemd-report` upload | `GET /api/v1/vms/{id}/report`, summarized by `get_vm_metrics`, as series on `/metrics` |
| Prometheus | `GET /metrics` (outside the OAuth guard; `--metrics-enabled=false` to turn it off) |

State directory layout, all below `/var/lib/vm-manager`:

```text
networks.json               network specs and leases
networks/<name>/qemu.sock   the network's unix socket QEMU attaches to
vms/<id>/vm.json            the VM record
vms/<id>/console.log        serial console
vms/<id>/user-data          Ignition JSON served as /user-data
vms/<id>/ssh_key            vm-manager's per-VM key for exec_vm, ssh_host_key the pinned guest key
vms/<id>/ovmf_vars.fd       per-VM OVMF variable store
vms/<id>/tpm/               swtpm state (the vTPM's identity; delete_vm removes it)
vms/<id>/qmp.sock           QMP socket while QEMU runs
vms/<id>/report.json        last guest report
volumes/                    VM disks when no systemd storage provider is present
images/                     the image directory of this guide
```

Keep the state directory path short: the unix socket paths below it are bounded by the
kernel's 108-byte limit.

## Upgrading images

A new base image version is a new `giantswarm-vm-base_<v>.efi` + `.raw` pair next to the
old one; `create_vm` without an explicit `image` takes the newest version, an explicit
`"image": "giantswarm-vm-base_<v>"` pins one. The `sysupdate/base/` directory is what the
guests' `systemd-sysupdate` (`/usr/lib/sysupdate.d/*.transfer`, A/B partitions) would
pull from `http://169.254.169.254/giantswarm/v1/sysupdate/base/`; the image ships the
transfers with `systemd-sysupdate.timer` disabled, so in-place OS updates are an explicit
`systemd-sysupdate update` inside the guest for now. Existing VMs keep the image they
installed; the intended upgrade path for cluster nodes is delete and recreate.

A new Kubernetes version is a new `kubernetes_<kv>.raw` in `sysupdate/kubernetes/`, listed
in its `SHA256SUMS` (signed as `SHA256SUMS.gpg` with the key the image trusts). The
directory is served unfiltered; each guest selects its version by reading
`/kubernetes-version` from the IMDS, so several versions can coexist and a VM pins its
own with `kubernetes_version`. `list_images` shows the versions the catalog found. Today's
`images/scripts/publish-sysupdate` writes a `SHA256SUMS` with only the version it just
built; a multi-version directory has to merge the manifests until
[wave 4, row 21](plan.md) lands.

`policy.json` is per image version: `make -C images verify` recomputes PCR 11 and 13 for
the new build and keeps the golden values of the same image version; a new version (or a
firmware update on the host) needs one learn-mode boot and `image golden` again.

## On Kubernetes (the chart)

[`helm/vm-manager`](../helm/vm-manager) is vm-manager as a platform pod, the shape the
[agent-platform](https://github.com/giantswarm/agent-platform) meta chart installs as
`components.vm-manager` (off by default; a node with KVM is the prerequisite). Released by
the generated CircleCI pipeline on every release tag: the image
`gsoci.azurecr.io/giantswarm/vm-manager:<version>` (Ubuntu 26.04's QEMU, swtpm and OVMF next
to the binary, x86-64), the chart `oci://gsoci.azurecr.io/charts/giantswarm/vm-manager:<version>`
in the giantswarm catalog, and the guest image the pod boots as the OCI artifact
`gsoci.azurecr.io/giantswarm/vm-manager-guest-image:<version>`.

What the chart renders, and why:

- A **Deployment** (one replica, `Recreate`) running `serve --launcher process
  --state-dir /var/lib/vm-manager --image-dir /var/lib/vm-manager/images`, **privileged**
  with `runAsUser: 0`. The container runtime gives a privileged container the node's
  devices with the device cgroup open, `/dev/kvm` and `/dev/vhost-vsock` among them, so
  nothing is mounted from the node — the chart has no hostPath; nothing else of the
  privilege is used (the networks are userspace: no bridges, no tap devices, no
  `CAP_NET_ADMIN`). A node without the devices starts the pod all the same; the host
  report names them under `missing`. A device plugin handing the two devices to an
  unprivileged pod is the refinement.
- No service manager in the pod, so QEMU and swtpm are children of the process and end
  with it: a pod restart stops the VMs. Their records, disks (plain files, no storage
  provider) and vTPM state survive on a claim (`persistence.existingClaim`, or
  `persistence.create: true`); without one the state is an emptyDir.
- The **guest image** is a published artifact: an init container runs `vm-manager image
  pull` before the server starts and fetches `guestImage.registry/repository:tag` (the
  chart's appVersion by default; `guestImage.digest` pins a manifest, `guestImage.plainHTTP`
  reaches a lab registry over HTTP, `guestImage.pullSecret` names a `dockerconfigjson`
  Secret for a private mirror) into `/var/lib/vm-manager/images` — the image directory is
  part of the state volume. A directory that already holds that digest
  (`.guest-image.json`) is left alone, so the golden PCR values `vm-manager image golden`
  records into its `policy.json` survive pod restarts on a claim; another digest replaces
  the contents. Recording golden values happens inside the pod, where the directory is
  writable: `kubectl exec deploy/vm-manager -- vm-manager image golden <id>_<version>
  --from-vm <id> --token <id_token>` (the server and image directory defaults are the
  pod's). `guestImage.enabled: false` fetches nothing (the chart's own install smoke).
- **OAuth** (`oauth.enabled`) against the platform identity: the issuer, client, client
  secret and CA fall back to `global.identity.*` and the base URL to `global.domain`, the
  way model-manager's chart reads them; `--allow-private-oauth-urls` and
  `--sso-allow-private-ips` for an in-cluster Dex are `oauth.dex.allowPrivateURLs` and
  `oauth.sso.allowPrivateIPs`. The trusted audiences are `oauth.trustedAudiences` (default
  the platform client) plus `muster.mcpServer.auth.requiredAudiences`.
- The **muster `MCPServer`** (`muster.mcpServer.enabled`): `streamable-http` to the
  Service, `auth.type: oauth` with `forwardToken: true`, labelled
  `agent-platform.giantswarm.io/tool-group: agent-platform` — the platform's own management
  surface, next to agent-manager and model-manager.
- The guests' traffic leaves through the pod's network (userspace NAT), so a network
  policy on the pod must admit their destinations: the chart's own policy
  (`networkPolicy.enabled`) admits every egress destination unless `guestEgress: false`.
- Resources: a memory limit bounds the sum of the VMs' memory too (they are processes of
  this container); the chart sets requests only.

Pod restarts and the golden values: the OVMF build inside the image differs from a host's,
so an image whose `policy.json` was recorded against another firmware needs `vm-manager
image golden` once against a VM the pod booted in learn mode (`vm.learnGolden: true` for
that one boot), run from a machine that has the image directory — the recipe of [First
VM](#first-vm) with the pod's API behind a port-forward or the platform's muster.

## Known limitations

- VMs survive service restarts only with a service manager for the vm-manager user
  (lingering, see above); without one they are child processes and stop with the service.
  A swtpm whose unit cannot be queried at reattach is treated as gone and no longer
  managed, even if the process is alive.
- One host per vm-manager; nothing schedules across hosts. The virtual network is
  userspace: hundreds of Mbit/s, IPv4 only, no UDP or ICMP from the host into the network.
- The attestation key is trusted on first use (vm-manager created the VM and its vTPM
  moments earlier); EK-certified keys and Secure Boot are follow-ups. PCR 12 (the stub's
  measurement of the extra command line and credentials) is quoted by neither agent stage
  and not part of the policy yet.
- `publish-sysupdate` keeps only the last published Kubernetes version in `SHA256SUMS`
  (above).
- A Kubernetes version change applies on the next boot of a VM, never to a running node.
- The Prometheus endpoint is unauthenticated by design; firewall it or turn it off where
  that matters.
