# vm-manager roadmap and agent plan

Companion to [design.md](design.md). Waves are approved one at a time before agents are
spawned; each row is one agent with one deliverable and a context budget of about 100k
tokens. Agents never poll: waits on CI or boots are bounded (interval + max attempts) and
return control to the parent.

## Status (2026-09-12, late)

Waves 1-3 complete. Wave 4: #33 README/design doc, install guide and systemd unit; #35 clean
var unmount with the sysext merged (exitrd waits for the ext4 superblock); #36 multi-version
Kubernetes sysexts (1.36.4 and 1.35.4 from the Arch archive); #37 VMs survive vm-manager
restarts (transient systemd services, persisted notify port, state-dir lock, reattach in
0.2 s). Row 18 (GitHub Actions image build + KVM e2e) is PR #34, in progress on the hosted
runners. Final verification on `main` after #37: all nine e2e tests green in 593 s on the
dev host (install 14 s, boot to READY 11-15 s, reboot 11 s, sysext pull 2 s, control plane
Ready 83 s, worker join 45 s, attestation learn/golden/tamper 110 s, reattach 0.2 s).

Follow-ups recorded by reviews and agents, not yet scheduled:
- `images/scripts/fetch-kubernetes-packages` caches by version, not `pkgver-pkgrel`; a
  republished pkgrel is never picked up on a warm cache.
- `vm-kubernetes.service` merges with `systemd-sysext refresh` on the first boot instead of
  starting `systemd-sysext.service`, so the manager phase does no unmerge there; starting the
  unit would make the no-pod shutdown fully synchronous.
- PCRs 1 and 5 (SMBIOS credentials, per-install GPT) and 12 (`ignition.firstboot`) are quoted
  but not compared; event-log replay is the proper fix.
- A swtpm that is alive but unreachable at reattach is treated as gone (VM runs without TPM).
- The caller identity is only logged; VM records carry no `requestedBy` unlike agent-manager.
- `.circleci/` is devctl-generated but CircleCI is not enabled; reconcile with the GitHub
  Actions pipeline of row 18.
- Nine sequential e2e tests take ~10 min; parallelise or shard when the suite grows.

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
