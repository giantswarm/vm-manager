# vm-manager roadmap and agent plan

Companion to [design.md](design.md). Waves are approved one at a time before agents are
spawned; each row is one agent with one deliverable and a context budget of about 100k
tokens. Agents never poll: waits on CI or boots are bounded (interval + max attempts) and
return control to the parent.

## Status (2026-09-12, end of wave 3)

Waves 1 to 3 are merged: #1-#19 (waves 1 and 2, incl. three CI flake fixes #17-#19 for
real races), then #20 attestation agent, #21 metrics + Prometheus, #22 quote verifier,
#23 Ignition in the initrd (gated user-data is a retryable 503, `/user-data` left the hwdb
table), #24 shared nonce store/AK template, #25 persistent `/etc`, #26 ssh host-key error
precedence, #27 Kubernetes sysext pulled at boot and measured into PCR 13, #28 vsock CID
collisions between vm-managers on one host, #29 plan status, #30 clean shutdown of the
`/etc` overlay via an exitrd, #32 attestation end to end (units in both stages,
`--attestation=verify` default, `image golden`, learn / golden / tamper e2e), #31 the
Kubernetes cluster e2e from CAPI-shaped Ignition. Every PR got an automated review before
its admin merge.

Last full `make e2e` on the development host (seven tests, about 6 min): install 12 s,
installed boot to `READY=1` 11-15 s, reboot to ready 11 s, Kubernetes sysext pull 2 s,
control plane Ready 85 s after its `create_vm`, worker joined 46 s after its own,
attestation learn / golden / tamper 111 s in total. Pending: the var-unmount fix
(`TestPersistentEtc` asserts that the rebooted system's fsck logs no `recovering journal`;
#30 introduced the exitrd for it and the assertion has to stay green with the Kubernetes
sysext merged).

Wave 4 is scheduled below (rows 18-21). Follow-ups recorded by reviews and agents:
- `images/scripts/publish-sysupdate` keeps only the last published Kubernetes version;
  multi-version fleets need it to accumulate versions (row 21).
- CI does not compile the `e2e` build tag (`go vet -tags e2e ./e2e/` broke twice unnoticed);
  add it to `make test` or the workflow (wave 4, row 18).
- ~~VMs still die with vm-manager; transient systemd units are the planned fix.~~ Done:
  QEMU and swtpm are transient systemd services, `serve` detaches on exit and reattaches
  on start (`e2e/restart_test.go`). Open: a system-service deployment that runs vm-manager
  as an unprivileged user needs a user manager for that user (`loginctl enable-linger`),
  else the launcher falls back to child processes.
- e2e `go test` timeout is 45 m for seven sequential tests; parallelise or split when it grows.
- PCR 12 (stub measurements of the extra command line and credentials) is neither quoted
  nor predicted; a predicted value would put `ignition.firstboot` under the policy.
- The muster `MCPServer` wiring for a service outside the cluster and the caller identity
  on VM records (`requestedBy`) are not done.

## Wave 1: repo, scaffold, image, first boot

| # | Deliverable | Inputs from parent | Est. context | Returns |
|---|---|---|---|---|
| 1 | Public repo `giantswarm/vm-manager` owned by team-bumblebee with this docs/ tree as the first commit, standard Giant Swarm repo setup (CODEOWNERS, labels, generated workflows) | local path, repo name, team, visibility public, git identity | ~30k | repo URL, list of generated files, default branch |
| 2 | Go scaffold in sibling layout (module `github.com/giantswarm/vm-manager`, cobra `serve`/`version`, `internal/server`, `internal/api` with `get_host` tool + REST, Makefiles, golangci, pre-commit, `docs/development.md`) with `make test lint` green, as a PR | repo URL, `sibling-conventions.md`, design.md, tool list | ~80k | PR link, `make test` + `make lint` output |
| 3 | `images/base` mkosi project producing `base_<v>.raw/.efi`, split root/verity, `SHA256SUMS(.gpg)`, `policy.json` stub; hwdb record, repart.sysinstall.d, credential-gated `vm-sysinstall.service`, sysupdate transfers, initrd with networkd + imdsd, dev-key generation; `make image image-verify` green, as a PR | repo URL, `systemd-research.md`, design.md image section, key policy | ~90k | PR link, artifact list with sizes, PCR 11 values, verify output |
| 4 | `images/kubernetes` sysext subimage (kubeadm, kubelet, containerd, runc, crictl, cni-plugins, units) in the same artifact layout, verified with `systemd-dissect`, as a PR | repo URL, row 3's mkosi layout and version scheme | ~60k | PR link, sysext size, extension-release content |
| 5 | `internal/runtime/qemu` + `internal/tpm`: phase A/B command builders, swtpm lifecycle, OVMF vars, QMP client, serial log, vsock READY listener, unit tests with fake exec, as a PR | repo URL, design.md boot flow, QEMU/OVMF/swtpm paths | ~70k | PR link, test output |
| 6 | e2e test (tag `e2e`): installer boot -> sysinstall -> reboot -> READY, hostname from `firstboot.hostname`, ssh over vsock works, boot times recorded; `make e2e` green locally, as a PR | rows 3 + 5 merged, artifact paths, budgets (60 s / 10 s) | ~60k | PR link, measured install and boot times |

Order: 1 -> (2 ∥ 3) -> (4 ∥ 5) -> 6. Rows 3, 4 and 6 need swtpm, mkosi and erofs-utils on
the host.

## Wave 2: network, IMDS, lifecycle, MCP

| # | Deliverable | Inputs | Est. | Returns |
|---|---|---|---|---|
| 7 | `internal/network` on gvisor-tap-vsock: network CRUD, DHCP leases by MAC, IMDS listener plumbing, `Dial`, port-forwards; unit tests with in-memory conns | design.md network section, gvisor API notes | ~70k | PR link |
| 8 | `internal/imds` Giant Swarm provider incl. attest/report/sysupdate endpoints over a VM-lookup interface; key table generates the hwdb record; httptest tests | design.md IMDS contract | ~60k | PR link |
| 9 | `internal/storage` + `internal/varlink` client for `io.systemd.StorageProvider.Acquire` with a fake Varlink server in tests | research section 3 | ~40k | PR link |
| 10a | `internal/vm` service + state machine + persistence + `internal/images` catalog, fakes for every dependency | APIs from rows 5, 6, 7, 8 | ~110k | PR link |
| 10b | Full MCP/REST tool set over the service, OpenAPI, `cmd/serve` wiring (storage detect, network restore, notify listener, IMDS servers, logrus redirect), contract tests, docs/development.md layout | row 10a merged | ~80k | PR link |
| 11 | e2e: `create_network` + `create_vm` via MCP client -> installed -> IMDS hostname and ssh key applied -> ssh via virtual network -> `delete_vm` | rows 6 + 10 merged | ~50k | PR link, timings |

Order: (7 ∥ 8 ∥ 9) -> 10a -> 10b -> 11.

## Wave 3: attestation, Kubernetes, metrics

| # | Deliverable | Inputs | Est. | Returns |
|---|---|---|---|---|
| 12 | `cmd/vm-agent`: `attest --stage=initrd\|ready` (nonce, AK, quote, event logs, POST); tests with the go-tpm simulator | attestation protocol | ~60k | PR link |
| 13 | Ignition in the mkosi initrd: binary from a pinned upstream release, `ignition-*` units for the systemd initrd, cmdline (`platform.id=metal`, `config.url`), first-boot flag via stub cmdline-extra from vm-manager; e2e: files + a unit from an Ignition config applied | rows 3, 5 merged, design.md image section | ~80k | PR link, Ignition version, e2e output |
| 14 | `internal/attest` verifier, `systemd-measure` PCR 11 computation, `vm-manager image golden`, user-data gating per stage | attestation protocol | ~70k | PR link |
| 15 | Image integration: agent units in initrd + system, report timer, kubernetes sysupdate component; e2e: attestation pass, tampered cmdline fails, sysext merged | rows 4, 12, 13, 14 merged | ~70k | PR link, PCR values |
| 16 | e2e: single-node kubeadm init from CAPI-shaped Ignition -> node Ready; then 1 control plane + 1 worker | row 15 merged | ~70k | PR link, join time |
| 17 | `internal/metrics`: host stats + guest report ingestion -> Prometheus + `get_vm_metrics` | row 10b merged, report JSON shape | ~50k | PR link |
| 12b | Persistent `/etc`: replace `systemd.volatile=overlay` with an `/etc` overlay whose upper dir lives in `/var`, set up in the initrd; e2e: a file written in `/etc` survives a reboot | row 6 merged, image deviations list | ~60k | PR link |
| 13b | Sysext measurement: decide ESP delivery vs `systemd-pcrextend` for PCR 13 and implement it; verify shows PCR 13 changes with the Kubernetes version | rows 13, 14 | ~50k | PR link |

Order: (12 ∥ 12b ∥ 13 ∥ 14) -> 13b -> 15 -> (16 ∥ 17).

## Wave 4: CI, docs, transient units, publishing

| # | Deliverable | Inputs | Est. | Returns |
|---|---|---|---|---|
| 18 | GitHub Actions: unit/lint with the `e2e` tag compiled, image build in an archlinux container with caches and artifacts, KVM e2e job, nightly full run; bounded waits | repo, Makefile targets | ~60k | workflow run links |
| 19 | README as the design doc (sibling structure), `docs/development.md` refreshed to `main`, `deploy/systemd/vm-manager.service` + sysusers and `docs/install.md`, design.md and this plan brought up to date | all merged work, the e2e numbers above | ~90k | PR link |
| 20 | Each VM as a transient systemd unit (`systemd-run` scope or service per VM with QEMU and swtpm), vm-manager re-dials the QMP socket and the notify stream on startup, `Load` reattaches instead of marking `stopped`; e2e: VMs survive a `vm-manager` restart | `internal/vm` invariants (`doc.go`), `internal/runtime/proc` | ~90k | PR link, restart e2e output |
| 21 | Multi-version publishing: `images/scripts/publish-sysupdate` accumulates `kubernetes_<kv>.raw` entries in one signed `SHA256SUMS`; the catalog and `get_image` list every version; e2e: two VMs on two Kubernetes versions from one image directory | images/README.md "Kubernetes sysext", `internal/images` | ~60k | PR link |

Order: (18 ∥ 19 ∥ 20 ∥ 21); 20 and 21 touch different packages than 19 (docs only).

Follow-ups outside the prototype: CAPI infrastructure provider / cluster-manager glue,
agent-platform wiring (muster MCPServer CR pointing at the host), a predicted PCR 12,
tap/bridge network backend, host-side pre-install fast path, EK-certified attestation
keys, secure boot, multi control plane endpoint (kube-vip).
