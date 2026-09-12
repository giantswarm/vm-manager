# Developing on vm-manager

```sh
make build          # binary for the current platform (./vm-manager)
make test           # unit + contract tests, race detector when a C toolchain is present
make test-race      # the same with -race forced
make test-integration # tests tagged `integration`: real QEMU + OVMF + swtpm on this host (skip without /dev/kvm)
make lint           # golangci-lint v2 with the pre-commit linters (gosec, goconst, govet)
make image          # mkosi build of the guest image into images/build/ (dev keys generated on first use)
make image-verify   # offline checks of the built image (GPT, UKI sections, expected PCR 11, hwdb, presets)
make e2e            # tests tagged `e2e` on KVM with the built image: install + reboot + READY over vsock, and network + IMDS through the MCP API of a real `vm-manager serve` (skip without it)
make serve          # go run . -v serve (SERVE_ARGS="--listen 127.0.0.1:18080" to override)
make help           # every target with its description
```

`make e2e` runs two tests. `TestInstallBoot` (`e2e/install_boot_test.go`) drives QEMU
directly with user networking: installer boot, sysinstall onto a blank disk, installed
boot to READY=1 and ssh over vsock; it passes `systemd.imds=no` because slirp offers no
IMDS. `TestNetworkIMDS` (`e2e/network_imds_test.go`) builds `vm-manager`, starts
`vm-manager serve` as a child process and drives it through the MCP endpoint:
`create_network`, `create_vm` with `wait_for: ready`, then proves over `exec_vm` that
the guest fetched hostname, ssh keys and instance id from the IMDS on the virtual
network and that `systemd-imds-import.service` succeeded, that
`systemctl is-system-running` is `running` with no failed unit, that the
`systemd-report` upload arrived (`get_vm_metrics`), that `forward_port` serves sshd,
and that `delete_vm`, `delete_network` and SIGTERM leave no qemu or swtpm behind.
Both print their timings for the budgets of [design.md](design.md) "Testing strategy":
`install_seconds=` and `boot_to_ready_seconds=` from the first, `api_create_vm_seconds=`,
`api_install_seconds=` and `api_boot_to_ready_seconds=` from the second.
`VM_MANAGER_E2E_IMAGE_DIR` points both at artifacts built elsewhere (default
`images/build`) and `VM_MANAGER_E2E_KEEP=1` keeps the per-test state directory
(consoles, TPM state, disks, the server log) after a pass. See `e2e/doc.go` for the
host requirements.

## Layout

- `cmd/` — cobra CLI (`serve`, `version`). Every `serve` flag has an
  environment fallback named in its help text (`VM_MANAGER_*`, plus the
  sibling-compatible `DEX_*` / `GOOGLE_*` / `OAUTH_TRUSTED_AUDIENCES`).
- `internal/host` — the host capability service: kernel, CPUs, memory,
  `/dev/kvm` and `/dev/vhost-vsock` access, qemu / swtpm / systemd versions,
  the OVMF code image, the `io.systemd.StorageProvider` sockets, and the
  `ready` / `missing` verdict. Commands go through an injectable `Runner`
  and file probes are relative to `Options.Root`, so tests run on a fixture
  tree without the VM stack.
- `internal/runtime/proc` — starts and supervises long-running host
  processes (QEMU, swtpm) behind the injectable `Exec` interface, the
  long-running counterpart of `host.Runner`: `OSExec` for the host,
  `FakeExec` for tests (records commands, the test drives exit codes and
  signals), plus `Tail`, the bounded stderr capture.
- `internal/runtime/qemu` — the VM process runtime. `Command(Spec)` is a
  pure function from a `Spec` (phase `install` or `boot`, UKI, installer
  DDI, target disk, netdevs, SMBIOS type 1, credentials as SMBIOS type 11
  `io.systemd.credential` strings, vsock CID, swtpm socket, OVMF paths,
  serial log, QMP socket) to the exact `qemu-system-x86_64` argv;
  `Runtime.Start` seeds the per-VM OVMF vars, launches QEMU and returns an
  `Instance` (`Wait`, `Stop` = QMP `system_powerdown` -> SIGTERM -> SIGKILL,
  `Kill`, `QMP`); `QMP` is the minimal client (capabilities, `query-status`,
  `system_powerdown`, `quit`, events); `NotifyListener` binds AF_VSOCK for
  the guest's `READY=1` and renders the `vmm.notify_socket` credential
  (`vsock-stream:2:<port>`). `integration_test.go` (tag `integration`)
  boots real OVMF with swtpm and user networking, no image needed.
- `internal/tpm` — one swtpm per VM: `Manager.Start` creates the state
  dir, runs `swtpm socket --tpm2` with a unixio control socket that ends
  the process when QEMU disconnects, waits for the socket, and `Instance`
  stops it.
- `internal/apierr` — the sentinel errors (`ErrNotFound`, `ErrInvalid`,
  `ErrConflict`, `ErrUnsupported`) domain packages wrap so both API surfaces
  answer the same status and code.
- `internal/api` — REST handlers (`rest.go`) and MCP tools (`mcp.go`) over
  the same service methods, wired through `api.Services`. `statusFor` maps
  sentinels to an HTTP status and a stable code; MCP tool errors carry the
  same code. `mcp_test.go` is the contract test: an mcp-go client over
  streamable HTTP against the assembled server.
- `internal/server` — the single HTTP listener (`/healthz`, `/readyz`,
  `/api/v1`, `/mcp`); with `--enable-oauth` the mcp-oauth resource server
  (Dex or Google) in front of REST and MCP.
- `internal/identity` — the authenticated caller on the request context.
- `internal/vm` — the VM service behind every VM and network tool: the
  lifecycle state machine (`creating` -> `installing` -> `booting` ->
  `attesting` -> `ready`, `running`, `stopping`, `stopped`, `failed`,
  `deleting`), persistence below `--state-dir` (`vms/<id>/vm.json`,
  `networks.json`), the orchestration of storage, swtpm, the network, the two
  QEMU phases and the IMDS (one server per network), `WaitFor` milestones,
  `Exec` over ssh and `Forward`. `vmtest/` holds the in-memory fakes of its
  dependencies, shared by the vm tests and the API contract tests.
- `internal/images` — the image catalog: scans `--image-dir` for
  `<id>_<version>.efi` + `.raw` pairs, the Kubernetes sysext versions under
  `sysupdate/kubernetes/SHA256SUMS` and the PCR policy files.
- `internal/network` — rootless virtual networks on gvisor-tap-vsock: DHCP
  leases, DNS, the IMDS alias 169.254.169.254, host port forwards, a dialer
  into the guest network for ssh, and persisted `State` for restarts.
- `internal/imds` — the instance metadata service handler (key table, the
  attestation protocol, `/user-data` gating, the report sink, the sysupdate
  tree); `internal/vm` resolves the caller's lease to its VM.
- `internal/storage` — volumes for VM disks: the systemd storage provider
  over varlink when its socket answers, plain files below
  `<state-dir>/volumes` otherwise (`storage.Detect`).
- `internal/varlink` — the minimal varlink client the storage provider uses.
- `internal/tpm` — one swtpm per VM (see above).
- `internal/runtime/proc`, `internal/runtime/qemu` — process supervision
  and the QEMU runtime (see above).
- `internal/attest` — planned: the quote verifier behind `imds.Attestor`;
  until it lands the service runs `imds.NoopAttestor`.
- `api/openapi.yaml` — the REST contract; served at `/api/v1/openapi.yaml`.
- `images/` — the mkosi build of the guest image and the Kubernetes sysext
  (`make -C images keys base`, outputs in `images/build/`; see
  `images/README.md`).

Domain packages plug in the same way as `internal/host`: a `Service` with
context-taking methods, a field on `api.Services`, one `s.AddTool` in
`NewMCPServer` and one route in `REST.Register` per operation, errors
wrapped from `internal/apierr`. `cmd/serve.go` wires them in the order
`internal/vm` documents: `storage.Detect` -> `network.NewManager` ->
`qemu.ListenNotify` -> `tpm.New` / `qemu.New` -> `images.Load` -> `vm.New`
-> `Load`, creates the `--default-network`, serves, and on shutdown stops
every VM (`Close`) within `--stop-timeout`. gvisor-tap-vsock logs through
logrus; `cmd/logrus.go` forwards it into slog and demotes its per-connection
teardown errors to debug.

## Adding a tool

1. Add the `Tool<Name>` constant and list it in `ToolNames()`.
2. `s.AddTool(mcp.NewTool(Tool<Name>, mcp.WithDescription("Read-only. ..." or
   "WRITES: ..."), args..., annotations...), t.<handler>)`; the handler returns
   `jsonResult(v)` or `errResult(err), nil`.
3. Mirror it in `REST.Register` with a Go 1.22 mux pattern and document it in
   `api/openapi.yaml`.
4. Extend the contract test's expected tool list and the README table.

## Local loop

```sh
make build
./vm-manager serve -v --listen 127.0.0.1:18080

curl -s localhost:18080/healthz
curl -s localhost:18080/api/v1/host | jq          # ready, missing, versions
curl -s localhost:18080/api/v1/openapi.yaml
```

The full loop with a guest, on a host where `GET /api/v1/host` says `ready`:

```sh
make -C images keys base                                  # images/build/giantswarm-vm-base_<v>.{efi,raw}
./vm-manager serve -v --listen 127.0.0.1:18080 --state-dir /tmp/vmm --image-dir images/build

API=localhost:18080/api/v1
curl -s $API/images | jq '.[].id'
curl -s -X POST $API/networks -d '{"name":"lab","cidr":"192.168.130.0/24"}' | jq .gateway
ID=$(curl -s -X POST $API/vms -d '{"name":"node-1","network":"lab","wait_for":"installed"}' | jq -r .id)
curl -s $API/vms/$ID | jq '{state, ip, lastError}'       # booting -> ready
curl -s "$API/vms/$ID/console?lines=40" | jq -r .console
curl -s -X POST $API/vms/$ID/exec -d '{"command":["systemctl","is-system-running"]}' | jq .
curl -s -X DELETE $API/vms/$ID -o /dev/null -w '%{http_code}\n'
curl -s -X DELETE $API/networks/lab -o /dev/null -w '%{http_code}\n'
```

`wait_for: ready` (the default) blocks until the guest sent `READY=1`;
`--install-timeout` and `--boot-timeout` bound it. The state directory holds
the console (`vms/<id>/console.log`) when something goes wrong in the guest.

The same operation as an MCP tool call over streamable HTTP (initialize, then
`tools/call`; the session id from the initialize response goes into
`Mcp-Session-Id`):

```sh
MCP=localhost:18080/mcp
H='-H content-type:application/json -H accept:application/json,text/event-stream'
SID=$(curl -si $H -X POST $MCP -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}' | awk 'tolower($1)=="mcp-session-id:"{print $2}' | tr -d '\r')
curl -s $H -H "mcp-session-id: $SID" -X POST $MCP -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
curl -s $H -H "mcp-session-id: $SID" -X POST $MCP -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
curl -s $H -H "mcp-session-id: $SID" -X POST $MCP -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_host"}}'
```

With `mcp-debug` or any MCP client, point it at `http://127.0.0.1:18080/mcp`.

## OAuth against a lab Dex

```sh
./vm-manager serve -v --listen 127.0.0.1:18080 --enable-oauth \
  --oauth-base-url http://localhost:18080 \
  --dex-issuer-url https://dex.lab.example/dex --dex-client-id agent-platform --dex-client-secret ... \
  --oauth-trusted-audiences agent-platform --allow-private-oauth-urls --sso-allow-private-ips
```

`/healthz`, `/readyz` and the OAuth metadata stay public; `/api/v1` and `/mcp`
require a bearer token: an id_token for a trusted audience (what muster
forwards) or a token from this server's own OAuth flow.
