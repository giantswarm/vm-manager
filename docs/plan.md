# vm-manager roadmap and agent plan

Companion to [design.md](design.md). Waves are approved one at a time before agents are
spawned; each row is one agent with one deliverable and a context budget of about 100k
tokens. Agents never poll: waits on CI or boots are bounded (interval + max attempts) and
return control to the parent.

## Status (2026-09-12)

Merged: #1 scaffold, #2 base image, #3 platform files, #4/#10 docs, #5 runtime, #6 IMDS,
#7 storage, #8 network, #9 Kubernetes sysext. In progress: row 6 (install-boot e2e) and
row 10a (VM service). Every PR gets an automated review before an admin merge.

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
| 12 | `cmd/vm-agent`: `attest --stage=initrd|ready` (nonce, AK, quote, event logs, POST); tests with the go-tpm simulator | attestation protocol | ~60k | PR link |
| 13 | Ignition in the mkosi initrd: binary from a pinned upstream release, `ignition-*` units for the systemd initrd, cmdline (`platform.id=metal`, `config.url`), first-boot flag via stub cmdline-extra from vm-manager; e2e: files + a unit from an Ignition config applied | rows 3, 5 merged, design.md image section | ~80k | PR link, Ignition version, e2e output |
| 14 | `internal/attest` verifier, `systemd-measure` PCR 11 computation, `vm-manager image golden`, user-data gating per stage | attestation protocol | ~70k | PR link |
| 15 | Image integration: agent units in initrd + system, report timer, kubernetes sysupdate component; e2e: attestation pass, tampered cmdline fails, sysext merged | rows 4, 12, 13, 14 merged | ~70k | PR link, PCR values |
| 16 | e2e: single-node kubeadm init from CAPI-shaped Ignition -> node Ready; then 1 control plane + 1 worker | row 15 merged | ~70k | PR link, join time |
| 17 | `internal/metrics`: host stats + guest report ingestion -> Prometheus + `get_vm_metrics` | row 10b merged, report JSON shape | ~50k | PR link |
| 12b | Persistent `/etc`: replace `systemd.volatile=overlay` with an `/etc` overlay whose upper dir lives in `/var`, set up in the initrd; e2e: a file written in `/etc` survives a reboot | row 6 merged, image deviations list | ~60k | PR link |
| 13b | Sysext measurement: decide ESP delivery vs `systemd-pcrextend` for PCR 13 and implement it; verify shows PCR 13 changes with the Kubernetes version | rows 13, 14 | ~50k | PR link |

Order: (12 ∥ 12b ∥ 13 ∥ 14) -> 13b -> 15 -> (16 ∥ 17).

## Wave 4: CI and docs

| # | Deliverable | Inputs | Est. | Returns |
|---|---|---|---|---|
| 18 | GitHub Actions: unit/lint, image build in an archlinux container with caches and artifacts, KVM e2e job, nightly full run; bounded waits | repo, Makefile targets | ~60k | workflow run links |
| 19 | README as design doc (sibling structure), `docs/development.md` local loops, `vm-manager.service` unit and install guide | all merged work | ~40k | PR link |

Follow-ups outside the prototype: CAPI infrastructure provider / cluster-manager glue,
agent-platform wiring (muster MCPServer CR pointing at the host), tap/bridge network
backend, host-side pre-install fast path, EK-certified attestation keys, secure boot.
