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

The remaining tools from the [design](docs/design.md#mcp-tool-surface-v1) (images,
networks, VMs) land with their domain packages.

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
