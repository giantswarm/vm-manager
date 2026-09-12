# vm-manager

Prototype. An MCP + REST server that provisions cloud-provider-like VMs (IMDS, vTPM with
measured boot, immutable mkosi-built OS, Kubernetes as a sysext layer) on a KVM host, so
agents on the Giant Swarm agent platform can hand them to the CAPI based cluster-manager.

Sibling of [agent-manager](https://github.com/giantswarm/agent-manager) and
[model-manager](https://github.com/giantswarm/model-manager).

- [Design](docs/design.md)
- [Roadmap and agent plan](docs/plan.md)
- [Development](docs/development.md)

## API at a glance

The same operations are exposed twice from one process: REST/JSON under `/api/v1`
(contract: [`api/openapi.yaml`](api/openapi.yaml), served at `/api/v1/openapi.yaml`) and
MCP tools over streamable HTTP at `/mcp` (through muster: `x_vm-manager_<tool>`). Both call
the same service and return the same JSON. Errors are `{"error":{"code","message"}}` with a
stable `code`; MCP tool errors carry the same code.

| Operation | REST | MCP tool | Writes |
|---|---|---|---|
| Host capabilities: kernel, CPUs, memory, `/dev/kvm`, `/dev/vhost-vsock`, qemu / swtpm / systemd versions, OVMF image, storage providers, `ready` + `missing` | `GET /api/v1/host` | `get_host` | no |
| List the bootable images (id, version, UKI, disk, Kubernetes sysext versions, PCR policy) | `GET /api/v1/images` | `list_images` | no |
| Describe one image (`<id>_<version>` or bare id = newest) | `GET /api/v1/images/{ref}` | `get_image` | no |
| List the virtual networks with gateway and leases | `GET /api/v1/networks` | `list_networks` | no |
| Describe one network | `GET /api/v1/networks/{name}` | `get_network` | no |
| List every VM record, oldest first | `GET /api/v1/vms` | `list_vms` | no |
| Fetch one VM: state, IP, attestation, last error | `GET /api/v1/vms/{id}` | `get_vm` | no |
| Tail of the serial console (`lines`, default 100) | `GET /api/v1/vms/{id}/console?lines=` | `get_vm_console` | no |
| The guest's last `systemd-report` upload (host metrics: later release) | `GET /api/v1/vms/{id}/metrics` | `get_vm_metrics` | no |
| The current boot's attestation verdicts | `GET /api/v1/vms/{id}/attestation` | `get_vm_attestation` | no |
| Create a network (`name`, `cidr`, `dns_search_domain`); IMDS always on | `POST /api/v1/networks` | `create_network` | yes |
| Delete a network; `conflict` while a VM is attached | `DELETE /api/v1/networks/{name}` | `delete_network` | yes, destructive |
| Create a VM (`name`, `image`, `kubernetes_version`, `cpus` 2, `memory_mib` 2048, `disk_gib` 20, `network` default, `user_data`, `ssh_authorized_keys`, `hostname`, `metadata`, `require_attestation` true, `wait_for` ready) | `POST /api/v1/vms` | `create_vm` | yes |
| Boot a stopped or failed VM from its disk | `POST /api/v1/vms/{id}/start` | `start_vm` | yes |
| Power a VM down (ACPI, then SIGTERM/SIGKILL after `--stop-timeout`) | `POST /api/v1/vms/{id}/stop` | `stop_vm` | yes |
| Stop, then start | `POST /api/v1/vms/{id}/reboot` | `reboot_vm` | yes |
| Stop if running, remove disk, vTPM state, lease and record | `DELETE /api/v1/vms/{id}` | `delete_vm` | yes, destructive |
| Run a command as root in the guest over ssh (`command` array) | `POST /api/v1/vms/{id}/exec` | `exec_vm` | yes, destructive |
| Expose a guest TCP port on a host loopback address (`port`) | `POST /api/v1/vms/{id}/forward` | `forward_port` | yes |

Request bodies use the tool argument names (snake_case); resources come back as the
service records them (camelCase). Error codes: `not_found`, `invalid_request`,
`conflict`, `unsupported`, `timeout` (a `wait_for` milestone was not reached; the VM keeps
running), `vm_failed` (the VM failed before the milestone; `lastError` and the console say
why), `internal_error`.

## Lifecycle

A VM needs a network: `create_network`, or the network named `default` that `serve`
creates at startup from `--network-subnet`. `create_vm` allocates a disk on the storage
provider, a vTPM, a lease and a vsock id, boots the installer (`installing`; sysinstall
copies the image onto the disk), reboots into the installed disk (`booting`), attests
from the initrd when `require_attestation` is set (`attesting`) and is `ready` once the
guest's service manager sent `READY=1` over vsock; `running` means the process is up but
`READY` never arrived within `--boot-timeout`. `wait_for` picks how long `create_vm`
blocks: `none` returns at once and `get_vm` follows the progress; `installed`, `attested`
or `ready` block for that milestone, bounded by `--install-timeout` and `--boot-timeout`.
`user_data` is CAPI's bootstrap Secret as Ignition JSON: the IMDS serves it at
`/user-data` only after a verified attestation quote from the initrd (immediately after the
installed boot starts when `require_attestation` is false), so a VM created without
`user_data` boots the bare OS. `stop_vm` / `start_vm` / `reboot_vm` move a VM between
`stopped` and running states (attestation and the user-data gate reset on every installed
boot); `delete_vm` removes the VM with its disk; `delete_network` succeeds once no VM is
attached. vm-manager does not reattach to QEMU across its own restarts: on shutdown every
VM is stopped gracefully, on startup such VMs are `stopped` with a note in `lastError`.

## Running

```sh
make build
./vm-manager serve                       # 127.0.0.1:8080, anonymous
./vm-manager serve --listen 0.0.0.0:8080 --enable-oauth --oauth-base-url https://vmm.example \
  --dex-issuer-url https://dex.example/dex --dex-client-id ... --dex-client-secret ... \
  --oauth-trusted-audiences agent-platform
```

Every flag has an environment fallback named in `vm-manager serve --help`
(`VM_MANAGER_LISTEN`, `VM_MANAGER_STATE_DIR`, `VM_MANAGER_IMAGE_DIR`,
`VM_MANAGER_NETWORK_SUBNET`, ...). State lives under `--state-dir`
(`$XDG_STATE_HOME/vm-manager`, else `~/.local/state/vm-manager`), images under
`--image-dir` (default `<state-dir>/images`); `/healthz` and `/readyz` are always open.

vm-manager runs on the KVM host itself (a laptop, a GitHub Actions runner, a bare-metal
node), not in a pod: `GET /api/v1/host` tells you whether the host is ready and what is
missing (`/dev/kvm`, `swtpm`, the systemd storage provider, ...).
