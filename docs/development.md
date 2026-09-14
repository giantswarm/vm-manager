# Developing on vm-manager

```sh
make build            # binary for the current platform (./vm-manager)
make test             # unit + contract tests, race detector when a C toolchain is present
make test-race        # the same with -race forced
make test-integration # tests tagged `integration`: real QEMU + OVMF + swtpm and the host's storage provider; each skips where its dependency is absent
make lint             # golangci-lint v2 with the pre-commit linters (gosec, goconst, govet)
make agent            # static guest binary bin/vm-agent (attest, pcrs, version); asserted static with file(1) and ldd(1)
make image            # mkosi build of the base image into images/build/ (dev keys generated on first use)
make image-verify     # offline checks of the built artifacts (GPT, UKI sections, expected PCR 11 and 13, hwdb, presets, sysext)
make e2e              # tests tagged `e2e` on KVM with the built image (go test -tags e2e -count=1 -timeout 45m -v ./e2e/...)
make serve            # go run . -v serve (SERVE_ARGS="--listen 127.0.0.1:18080" to override)
make help             # every target with its description
```

`make image` is `make -C images keys base`; the Kubernetes sysext and the full artifact
set come from `make -C images` (see [images/README.md](../images/README.md)). The image
build stages `bin/vm-agent` itself (`images/Makefile` runs `make -C .. agent`), so a
changed agent changes the initrd and with it PCR 11.

## End-to-end tests

`make e2e` runs seven tests sequentially on the host's KVM against `images/build`
(`VM_MANAGER_E2E_IMAGE_DIR` points elsewhere); the whole suite takes about 6 min on the
development host and `go test` gets a 45 m ceiling. A host without `/dev/kvm`,
`/dev/vhost-vsock`, `qemu-system-x86_64`, `swtpm` 0.8+, OVMF (Arch `edk2-ovmf` or Ubuntu
`ovmf`), `sfdisk` or the artifacts skips with a message naming what is missing
(`e2e/doc.go`); ssh into the guests is dialed over AF_VSOCK from Go, so no ssh client or
`systemd-ssh-proxy` is needed. Every test prints its timings as `<name>_seconds=` lines; the numbers
below are the last full run on the development host.

| Test | Drives | Proves | Measured |
|---|---|---|---|
| `TestInstallBoot` | QEMU directly (`internal/runtime/qemu`, `internal/tpm`) with user networking and `systemd.imds=no` | installer boot, sysinstall onto a blank disk, partition UUIDs, installed boot to `READY=1`, ssh over vsock | `install_seconds=12`, `boot_to_ready_seconds=11-15` |
| `TestNetworkIMDS` | `vm-manager serve` as a child process through `/mcp` | `create_network`, `create_vm` with `wait_for: ready`; hostname, ssh key and instance id from the IMDS; `is-system-running` clean; `systemd-report` upload arrived; `forward_port` serves sshd; `delete_vm`, `delete_network`, SIGTERM leave no qemu or swtpm | `api_install_seconds`, `api_boot_to_ready_seconds` in the same range |
| `TestIgnition` | MCP | user-data as Ignition JSON: the first installed boot applies its files and unit from the initrd stages, a reboot leaves Ignition idle | first installed boot |
| `TestPersistentEtc` | MCP | a file and an enabled unit in `/etc` survive `reboot_vm`; host key and machine ID unchanged; var unmounted cleanly (no `recovering journal`) | `reboot_to_ready_seconds=11` |
| `TestKubernetesSysext` | MCP | `vm-kubernetes.service` pulls the sysext, merges it, containerd and kubelet up, PCR 13 equals `policy.json` | `kubernetes_pull_seconds=2`, `kubernetes_unit_seconds=2.7` |
| `TestAttestation` | three servers in turn: learn mode, verify with the recorded golden values, verify on `OVMF_CODE.secboot.4m.fd` | verified initrd and ready quotes, user-data applied; `golden mismatch` on PCR 0 and 7 with the other firmware, user-data gated, Ignition in its fetch loop | 111 s for the three |
| `TestKubernetesCluster` | MCP plus the host's `kubectl` through `forward_port` 6443 | control plane from CAPI-shaped Ignition (`kubeadm init` ordered `After=vm-kubernetes.service`), flannel, a worker joined with a bootstrap token, two Ready nodes with `giantswarm-vm://` providerIDs, cross-node DNS, clean units and unmanaged CNI links | `cp_ready_seconds=85`, `worker_join_seconds=46`, 2.5 to 3 min in total |

`TestKubernetesCluster` needs `kubectl` on the host and internet access from the VMs
(registry.k8s.io, ghcr.io, github.com for the flannel manifest) and raises
`VM_MANAGER_BOOT_TIMEOUT` for its server because `READY=1` waits for the kubeadm unit.
On failure the per-test state directory (consoles, TPM state, disks, the server log) is
kept and its path printed; `VM_MANAGER_E2E_KEEP=1` keeps it after a pass too.

## Layout

- `cmd/` — cobra CLI: `serve` (every flag has an environment fallback named in
  its help text, `VM_MANAGER_*` plus the sibling-compatible `DEX_*`, `GOOGLE_*`,
  `OAUTH_TRUSTED_AUDIENCES`, `SSO_ALLOW_PRIVATE_IPS`), `image golden` (records
  golden PCRs from a VM's verified quote into the image's `policy.json`),
  `version`; `cmd/vm-agent` is the guest binary (`attest --stage=initrd|ready`,
  `pcrs`, `version`). `cmd/logrus.go` forwards gvisor-tap-vsock's logrus output
  into slog and demotes its per-connection teardown errors to debug.
- `internal/agent` — the guest side of attestation: nonce, AK, `TPM2_Quote` over
  `/dev/tpmrm0`, event logs, the POST to the IMDS; `agent/quote` is the TPM
  plumbing (keys, PCR reads, quotes), tested against swtpm and the go-tpm
  simulator.
- `internal/api` — REST handlers (`rest.go`) and MCP tools (`mcp.go`) over the
  same service methods, wired through `api.Services`; `vm.go` holds the create
  bodies and defaults shared by both. `statusFor` maps the `apierr` sentinels to
  an HTTP status and a stable code; MCP tool errors carry the same code.
  `mcp_test.go` is the contract test: an mcp-go client over streamable HTTP
  against the assembled server.
- `internal/apierr` — the sentinel errors (`ErrNotFound`, `ErrInvalid`,
  `ErrConflict`, `ErrUnsupported`) domain packages wrap so both API surfaces
  answer the same status and code; `statusFor` also maps the VM service's
  milestone errors `vm.ErrTimeout` (504 `timeout`) and `vm.ErrFailed` (500
  `vm_failed`).
- `internal/attest` — the `imds.Attestor` behind `--attestation=verify` (the
  default): nonces, AK pinning per VM on the first verified initrd quote, PCR 11
  against the image's `policy.json` phase paths, PCRs 0, 2-4, 6, 7 (and 13 at
  ready, `SysextPCR`) against its golden values; PCR 1 and 5 are recorded, not
  compared. `--attestation-learn-golden` accepts and records missing golden
  values for `vm-manager image golden`. `--attestation=noop` runs
  `imds.NoopAttestor`, which verifies nothing.
- `internal/host` — the host capability service behind `get_host`: kernel, CPUs,
  memory, `/dev/kvm` and `/dev/vhost-vsock` access, qemu / swtpm / systemd
  versions, the OVMF code image, the `io.systemd.StorageProvider` sockets, and
  the `ready` / `missing` verdict. Commands go through an injectable `Runner`,
  file probes are relative to `Options.Root`, so tests run on a fixture tree.
- `internal/identity` — the authenticated caller on the request context
  (subject, email, groups, source `sso|oauth`).
- `internal/images` — the image catalog: scans `--image-dir` for
  `<id>_<version>.efi` + `.raw` pairs, `policy.json`, and the Kubernetes sysext
  versions under `sysupdate/kubernetes/SHA256SUMS`.
- `internal/imds` — the instance metadata service handler: the key table (single
  source of truth for the hwdb record in `images/`, `TestHwdbRecordParity`), the
  attestation endpoints, `/user-data` gating with Ignition's status codes, the
  report sink, the sysupdate tree; `internal/vm` resolves a caller's lease IP to
  its VM.
- `internal/metrics` — the Prometheus registry: host metrics over the VM service
  (`metrics.Source`), the guests' `systemd-report` uploads (`StoreReport`, one
  series per entry with a per-VM cap) and the per-VM summary of `get_vm_metrics`.
  `doc.go` documents the mapping and the cardinality policy.
- `internal/network` — rootless virtual networks on gvisor-tap-vsock: one L2
  segment per network, DHCP with static leases derived from the MAC, DNS, NAT,
  the IMDS virtual IP 169.254.169.254, the opt-in host alias, host port
  forwards, a dialer into the network for ssh, and persisted `State` for
  restarts (`doc.go` has the addressing).
- `internal/nonce` — the one nonce store (5 min TTL, single use) the IMDS hands
  to both attestors.
- `internal/runtime/proc` — starts and supervises long-running host
  processes (QEMU, swtpm) behind the injectable `Exec` interface, the
  long-running counterpart of `host.Runner`. `SystemdExec` runs each
  command as a transient systemd service (`systemd-run --service-type=exec
  -p RemainAfterExit=yes`, user or system manager) that outlives
  vm-manager and can be picked up again by `Attach` from the persisted
  `Handle` (unit name + PID; exit via a pidfd, status from `systemctl
  show`); `SelectLauncher` chooses it when a manager is reachable and
  `OSExec` (plain children) otherwise. `FakeExec` is for tests (records
  commands, the test drives exit codes and signals; it attaches by PID),
  `Tail`/`TailFile` the bounded output capture. The integration test
  `TestIntegrationSystemdExec` drives the host's real user manager.
- `internal/runtime/qemu` — the VM process runtime. `Command(Spec)` is a pure
  function from a `Spec` (phase `install` or `boot`, UKI, installer DDI, target
  disk, netdevs, SMBIOS type 1, credentials as SMBIOS type 11
  `io.systemd.credential` strings, the `io.systemd.stub.kernel-cmdline-extra`
  string, vsock CID, swtpm socket, OVMF paths, serial log, QMP socket) to the
  exact `qemu-system-x86_64` argv; `Runtime.Start` seeds the per-VM OVMF vars,
  launches QEMU and returns an `Instance` (`Wait`, `Stop` = QMP
  `system_powerdown` -> SIGTERM -> SIGKILL, `Kill`, `QMP`); `NotifyListener`
  binds AF_VSOCK for the guest's `READY=1` and renders the `vmm.notify_socket`
  credential (`vsock-stream:2:<port>`). `integration_test.go` boots real OVMF
  with swtpm and user networking, no image needed.
- `internal/server` — the single HTTP listener (`/healthz`, `/readyz`,
  `/metrics`, `/api/v1`, `/mcp`); with `--enable-oauth` the mcp-oauth resource
  server (Dex or Google) in front of REST and MCP; probes and `/metrics` stay
  open.
- `internal/storage` — volumes for VM disks: the systemd storage provider over
  varlink when a socket under `/run/systemd/io.systemd.StorageProvider/`
  answers `GetInfo`, plain files below `<state-dir>/volumes` otherwise
  (`storage.Detect`).
- `internal/tpm` — one swtpm per VM: `Manager.Start` creates the state dir, runs
  `swtpm socket --tpm2` with a unixio control socket that ends the process when
  QEMU disconnects, waits for the socket; `Instance` stops it.
- `internal/tpmquote` — pure parsing and cryptographic verification of a
  `TPM2_Quote` (TPMS_ATTEST, TPMT_SIGNATURE, AK TPMT_PUBLIC): magic and type,
  AK attributes, signature, nonce, PCR digest. `tpmquote/quotetest` produces
  real quotes on the go-tpm simulator (cgo) for tests.
- `internal/varlink` — the minimal varlink client (JSON over AF_UNIX, passed
  descriptors) the storage provider uses.
- `internal/vm` — the VM service behind every VM and network tool: the
  lifecycle state machine (`doc.go` has the states, transitions, milestones and
  the state directory), persistence below `--state-dir`, the orchestration of
  storage, swtpm, the network, the two QEMU phases and one IMDS server per
  network, `WaitFor`, `Exec` over ssh with a per-VM key and a pinned host key,
  `Forward`, the attestation hooks and `PolicyFor`. `vmtest/` holds the
  in-memory fakes of its dependencies, shared by the vm tests and the API
  contract tests.
- `api/openapi.yaml` — the REST contract; embedded and served at
  `/api/v1/openapi.yaml`.
- `images/` — the mkosi build of the guest image and the Kubernetes sysext
  ([images/README.md](../images/README.md)).
- `e2e/` — the T3 boot tests above; `harness.go` builds and starts `vm-manager
  serve` and speaks MCP to it.
- `deploy/systemd/` — the system service unit and sysusers file of
  [install.md](install.md).

Domain packages plug in the same way as `internal/host`: a `Service` with
context-taking methods, a field on `api.Services`, one `s.AddTool` in
`NewMCPServer` and one route in `REST.Register` per operation, errors
wrapped from `internal/apierr`. `cmd/serve.go` wires them in the order
`internal/vm` documents: `storage.Detect` -> `network.NewManager` ->
`qemu.ListenNotify` -> `proc.SelectLauncher` -> `tpm.New` / `qemu.New` ->
`images.Load` -> `vm.New` -> `Load` (which reattaches VMs an earlier server
left running), creates the `--default-network`, serves, and on shutdown
leaves the VMs to the next server (`--detach-vms-on-exit`, the default with
the systemd launcher) or stops every one (`Close`) within `--stop-timeout`.
`e2e/restart_test.go` proves the handover end to end and prints
`reattach_seconds`. gvisor-tap-vsock logs through
logrus; `cmd/logrus.go` forwards it into slog and demotes its per-connection
teardown errors to debug.

## Adding a tool

1. Add the `Tool<Name>` constant and list it in `ToolNames()` (registration
   order: reads first, then writes).
2. `s.AddTool(newTool(Tool<Name>, "Read-only. ..." or "WRITES: ..." or
   "WRITES (destructive): ...", hintRead|hintCreate|hintIdempotent|hintDestructive|hintExec,
   args...), t.<handler>)`; the handler returns `jsonResult(v)` or
   `errResult(err), nil`. The description says what to call first and what the
   result means; the hints set all four MCP annotations.
3. Mirror it in `REST.Register` with a Go 1.22 mux pattern and document it in
   `api/openapi.yaml`; request bodies use the tool's snake_case argument names.
4. Extend the contract test's expected tool list (`mcp_test.go`), the README's
   "API at a glance" table and, when it is a VM operation, the `instructions`
   paragraph in `mcp.go`.

## Local loop

```sh
make build
./vm-manager serve -v --listen 127.0.0.1:18080

curl -s localhost:18080/healthz
curl -s localhost:18080/api/v1/host | jq          # ready, missing, versions
curl -s localhost:18080/api/v1/openapi.yaml
```

The full loop with a guest, on a host where `GET /api/v1/host` says `ready`. A fresh
`images/build/policy.json` has no golden PCR values and the default
`--attestation=verify` rejects every quote without them, so the first server runs in
learn mode; `vm-manager image golden` records the values of one attested VM and the
server is restarted without the flag (`e2e/attestation_test.go` does the same):

```sh
make -C images                                            # images/build/giantswarm-vm-base_<v>.{efi,raw}, sysupdate/, policy.json
./vm-manager serve -v --listen 127.0.0.1:18080 --state-dir /tmp/vmm --image-dir images/build \
    --attestation-learn-golden                            # bring-up only, until image golden ran

API=localhost:18080/api/v1
curl -s $API/images | jq '.[] | {id, version, kubernetesVersions}'
curl -s -X POST $API/networks -d '{"name":"lab","cidr":"192.168.130.0/24"}' | jq .gateway
ID=$(curl -s -X POST $API/vms -d '{"name":"node-1","network":"lab","wait_for":"installed"}' | jq -r .id)
curl -s $API/vms/$ID | jq '{state, ip, lastError}'       # booting -> attesting -> ready
curl -s $API/vms/$ID/attestation | jq                     # initrd and ready quote, learned PCRs
./vm-manager image golden giantswarm-vm-base --from-vm $ID --server http://127.0.0.1:18080 --image-dir images/build
# restart serve without --attestation-learn-golden: later VMs verify against the recorded values
curl -s "$API/vms/$ID/console?lines=40" | jq -r .console
curl -s -X POST $API/vms/$ID/exec -d '{"command":["systemctl","is-system-running"]}' | jq .
curl -s -X DELETE $API/vms/$ID -o /dev/null -w '%{http_code}\n'
curl -s -X DELETE $API/networks/lab -o /dev/null -w '%{http_code}\n'
```

`wait_for: ready` (the default) blocks until the guest sent `READY=1`;
`--install-timeout` (5 m) and `--boot-timeout` (4 m) bound it. Keep `--state-dir` short:
unix socket paths below it are limited to 108 bytes.

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

## Debugging a guest

- **The serial console first.** `get_vm_console` (`GET /vms/{id}/console?lines=`) or
  the file `<state-dir>/vms/<id>/console.log`. Installer failures put the console tail
  into `lastError`; the `ignition-*` and `vm-agent-attest.service` units log to
  `journal+console`, so a fetch loop or a `golden mismatch: pcr 0 expected ..., got ...`
  is visible there without ssh.
- **Inside the guest.** `exec_vm` runs as root over ssh through the virtual network:
  `["journalctl","-b","-u","vm-kubernetes.service"]`, `["systemctl","--failed"]`,
  `["systemd-analyze","pcrs","11","13"]`, `["cat","/run/log/systemd/tpm2-measure.log"]`,
  `["systemd-imds","/kubernetes-version"]`. `forward_port` with `22` gives a plain ssh
  session (`ssh -i <state-dir>/vms/<id>/ssh_key -p <port> root@127.0.0.1`).
- **Attestation.** `get_vm_attestation` shows both stages with the PCRs the agent quoted
  and the verifier's reason; compare against `policy.json` (`pcr11` per phase path,
  `pcr13` per Kubernetes version, `golden.sha256`). A server in learn mode logs a warning
  at startup and reports learned PCRs on the VM.
- **e2e leftovers.** `VM_MANAGER_E2E_KEEP=1 make e2e` keeps every test's state directory
  (consoles, TPM state, disks, the server's log) and prints the path; without it only
  failing tests keep theirs.
- **A boot outside vm-manager.** `make -C images smoke-boot` (systemd-vmspawn, no IMDS:
  the IMDS, attestation and Kubernetes units skip themselves) and `PROFILE=debug` for
  root autologin on the console.
- **A vm-manager restart** marks every VM that was running as `stopped` with a note in
  `lastError`; `start_vm` boots them again.

## OAuth against a lab Dex

```sh
./vm-manager serve -v --listen 127.0.0.1:18080 --enable-oauth \
  --oauth-base-url http://localhost:18080 \
  --dex-issuer-url https://dex.lab.example/dex --dex-client-id agent-platform --dex-client-secret ... \
  --oauth-trusted-audiences agent-platform --allow-private-oauth-urls --sso-allow-private-ips
```

`/healthz`, `/readyz`, `/metrics` and the OAuth metadata stay public; `/api/v1` and `/mcp`
require a bearer token: an id_token for a trusted audience (what muster forwards) or a
token from this server's own OAuth flow. `vm-manager image golden --token` (or
`VM_MANAGER_TOKEN`) talks to such a server.

## CI

GitHub Actions, `.github/workflows/`. The devctl-generated `zz_generated.*` workflows add
pre-commit, gitleaks, semantic PR titles and the release automation. CircleCI generation is
switched off for this repo in giantswarm/github (`gen.ci.generate: false` in
`repositories/team-bumblebee.yaml`): the image build needs an Arch container and the boot
tests nested KVM, and the container image and chart publish from here to ghcr.io
(`publish.yml`), the way the kagent and Substrate lines do. The tiers are those of
[design.md](design.md) "Testing strategy".

| Workflow | Runs on | When | What |
|---|---|---|---|
| `test.yml` | `ubuntu-latest` | every PR, push to main | `make test vet-e2e` (T0/T1, plus `go vet -tags e2e ./e2e/...` so the e2e package cannot rot unnoticed) and `make lint lint-e2e` |
| `image.yml` | `archlinux:latest` container (`--privileged`) on `ubuntu-24.04` | PRs touching `images/**`, `cmd/vm-agent/**`, `internal/agent/**` or the workflow; push to main with the same paths; manual; called by `e2e.yml` | `make -C images` (all: keys, base image, every Kubernetes sysext of `KUBERNETES_VERSIONS`, verify; T2), sizes and the expected PCR 11 values in the job summary, `images/build/` (UKI, disk image, split partitions, `sysupdate/`, `policy.json`; not `base/`) as the **`guest-image`** artifact, 7 days (30 on main) |
| `e2e.yml` | `ubuntu-24.04` (nested KVM) | every PR, push to main: **fast** subset; nightly 02:17 UTC and manual: **full** suite | T3: `go test -tags e2e` against the artifact, consoles and logs as the **`e2e-logs-<suite>`** artifact |
| `chart.yml` | `ubuntu-latest` | every PR, push to main | the pod shape: `make helm-lint helm-verify` (`hack/verify-chart.sh` render assertions), `values.schema.json` and the chart README current, then the image of the commit built and side-loaded into a kind cluster and the chart installed with `helm/vm-manager/ci/smoke-values.yaml` (devices off, no KVM on the runner): the Deployment ready, `GET /api/v1/host` naming `/dev/kvm` under `missing` with QEMU, swtpm and OVMF found |
| `publish.yml` | `ubuntu-latest` | after every green Auto-release run on main (a `workflow_run`: the tag is pushed with the workflow token, which fires no `push` event), a hand-pushed `v*` tag, or manual with a version | `ghcr.io/giantswarm/vm-manager:<version>` (linux/amd64) and the chart `oci://ghcr.io/giantswarm/vm-manager/helm/vm-manager:<version>`, version and appVersion stamped from the tag |

The image build runs in an Arch container because the image is Arch (mkosi 27, systemd 261,
erofs-utils, ukify, systemd-measure) and the hosted Ubuntu runner has none of that at the needed
versions. `--privileged` gives mkosi's sandbox its mount namespaces; the mkosi workspace is
placed on the runner's bind-mounted temp directory because the sysext's overlayfs cannot stack on
the container's overlay root. `images/mkosi.cache` (incremental trees) and `~/.cache/mkosi`
(pacman packages) are cached with `actions/cache`, keyed on the hash of the mkosi configuration
and scripts plus an ISO-week stamp: within a week the cache is reused (a warm build takes about a
minute, a cold one three to five), a new week starts from the current Arch repositories.

`e2e.yml` has three jobs. `plan` decides the suite and where the image comes from: a PR or push
that touches the image inputs builds it in this run (`image.yml` via `workflow_call`); otherwise
the newest unexpired `guest-image` of a green `image.yml` or `e2e.yml` run on `main` is reused
(and if there is none, it builds). Nightly and manual runs always build; the `image_run_id` input
of a manual run reuses that run's artifact instead. `test` enables `/dev/kvm` (udev rule
`99-kvm4all.rules`, mode 0666) and `vhost_vsock` (`modprobe`, chmod), installs `qemu-system-x86`
and `ovmf` from apt and `swtpm` from `ppa:stefanberger/swtpm-noble` (noble's swtpm 0.7.3 rejects
the `terminate` ctrl option of swtpm 0.8+ that `internal/tpm` passes) and unloads Ubuntu's
AppArmor profile for swtpm, which denies sockets and state outside its allowed paths (the
per-test directories under `/mnt/e2e` are). Noble's QEMU is 8.2.2: `internal/runtime/qemu`
probes the release once and passes the network's `-netdev stream` re-dial option as
`reconnect=<s>` there instead of the `reconnect-ms=` of QEMU 9.2+ (the runner's systemd 255
has no `systemd-ssh-proxy` and no storage provider: the harness dials ssh over AF_VSOCK from Go
and vm-manager falls back to file-backed volumes), downloads the artifact to `/mnt/e2e/image` and
runs the tests with `TMPDIR=/mnt/e2e` (the runner's large data disk, short socket paths) and
`VM_MANAGER_E2E_KEEP=1`.

Fast subset (PRs, 40-minute job timeout): `TestInstallBoot`, `TestNetworkIMDS`,
`TestPersistentEtc`, `TestKubernetesSysext`. Full suite (nightly, 60 minutes): those plus
`TestIgnition`, `TestKubernetesVersions` (one VM per published sysext version),
`TestAttestation` (the tamper case uses Ubuntu's `OVMF_CODE_4M.secboot.fd`) and
`TestKubernetesCluster` (needs the runner's `kubectl` and internet access from the VMs). The job
summary lists the results and the `*_seconds=` budget lines.

Rerun: `gh run rerun <id> --failed`, or `gh workflow run e2e.yml --ref <branch> -f suite=fast`
(`-f image_run_id=<id>` to reuse a specific image) and `gh workflow run image.yml --ref <branch>`.
Failures: `gh run view <id> --log-failed`, then the `e2e-logs-<suite>` artifact (`gh run download
<id> -n e2e-logs-fast`) for the serial consoles (`<phase>.log`, `vms/<id>/console.log`) and the
server logs of the failed test's `vmm-e2e-*` directory. A PR that conflicts with `main` gets no
`pull_request` runs at all (GitHub cannot create its merge commit): rebase first.

## Testing against the agent platform (agentlab)

[agentlab](https://github.com/giantswarm/agentlab) runs the whole agent platform — Dex,
the agentgateway edge, muster, Backstage, kagent — on a local kind cluster and, with
`platform.vmManager` in `agentlab.yaml`, the agent-platform chart's `components.vm-manager`
in it: this chart, as a pod of the kind node (a privileged docker container, so the host's
`/dev/kvm` and `/dev/vhost-vsock` are in it), with the image directory of this checkout
mounted into the node (`platform.vmManager.imageDir`) and a build of this checkout swapped
in through the lab's dev-image loop (`platform.devImages.vm-manager`). That is the
end-to-end test of this repo's platform surface: the person's Dex id_token forwarded by
muster and validated here, the tools aggregated as `x_vm-manager_<tool>` with their
annotations, the portal listing the server under Agent Platform, and a VM created,
followed to `ready`, attested and deleted through muster.

```sh
make -C images                                       # once: the image directory the pod mounts
make docker-build TAG=vm-manager:dev                 # the image of this checkout
cd ~/projects/giantswarm/agentlab
agentlab configure --vm-manager --vm-manager-image-dir ~/projects/giantswarm/vm-manager/images/build
# platform.devImages: {vm-manager: vm-manager:dev} in agentlab.yaml swaps the build in
agentlab up                                          # the image-dir mount is fixed at kind create: `agentlab down && up` after changing it
agentlab vm-manager-test                             # 401 anonymous -> token accepted -> tools -> create_vm -> ready -> attestation -> delete_vm
```

The pod's OVMF is Ubuntu's, not the host's: an image whose `policy.json` was recorded on
the host needs `vm-manager image golden` once against a VM the pod booted in learn mode
(agentlab's docs/vm-manager.md has the recipe), and the proof boots its VM with
`require_attestation: true` from then on.
