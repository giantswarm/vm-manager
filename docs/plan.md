# vm-manager roadmap and agent plan

Companion to [design.md](design.md). Waves are approved one at a time before agents are
spawned; each row is one agent with one deliverable and a context budget of about 100k
tokens. Agents never poll: waits on CI or boots are bounded (interval + max attempts) and
return control to the parent.

## Wave 1: repo, scaffold, image, first boot

| # | Deliverable | Inputs from parent | Est. context | Returns |
|---|---|---|---|---|
| 1 | Public repo `giantswarm/vm-manager` owned by team-bumblebee with this docs/ tree as the first commit, standard Giant Swarm repo setup (CODEOWNERS, labels, generated workflows) | local path, repo name, team, visibility public, git identity | ~30k | repo URL, list of generated files, default branch |
| 2 | Go scaffold in sibling layout (module `github.com/giantswarm/vm-manager`, cobra `serve`/`version`, `internal/server`, `internal/api` with `get_host` tool + REST, Makefiles, golangci, pre-commit, `docs/development.md`) with `make test lint` green, as a PR | repo URL, `sibling-conventions.md`, design.md, tool list | ~80k | PR link, `make test` + `make lint` output |
| 3 | `images/base` mkosi project producing `base_<v>.raw/.efi`, split root/verity, `SHA256SUMS(.gpg)`, `policy.json` stub; hwdb record, repart.sysinstall.d, credential-gated `vm-sysinstall.service`, sysupdate transfers, dev-key generation; `make image image-verify` green, as a PR | repo URL, `systemd-research.md`, design.md image section, key policy | ~90k | PR link, artifact list with sizes, PCR 11 values, verify output |
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
| 10 | `internal/vm` service + state machine + persistence + full MCP/REST tool set + contract tests | interfaces from rows 5, 7, 8, 9 | ~90k | PR link |
| 11 | e2e: `create_network` + `create_vm` via MCP client -> installed -> IMDS hostname and ssh key applied -> ssh via virtual network -> `delete_vm` | rows 6 + 10 merged | ~50k | PR link, timings |

Order: (7 ∥ 8 ∥ 9) -> 10 -> 11.

## Wave 3: attestation, Kubernetes, metrics

| # | Deliverable | Inputs | Est. | Returns |
|---|---|---|---|---|
| 12 | `cmd/vm-agent`: `attest`, `bootstrap` (cloud-config subset), `ready`; tests with go-tpm simulator and CAPI fixtures | attestation protocol, cloud-config subset spec | ~80k | PR link |
| 13 | `internal/attest` verifier, `systemd-measure` PCR 11 computation, `vm-manager image golden`, user-data gating | attestation protocol | ~70k | PR link |
| 14 | Image integration: agent units, report timer, kubernetes sysupdate component; e2e: attestation pass, tampered cmdline fails, sysext merged | rows 4, 12, 13 merged | ~70k | PR link, PCR values |
| 15 | e2e: single-node kubeadm init from CAPI-shaped cloud-config -> node Ready; then 1 control plane + 1 worker | row 14 merged | ~70k | PR link, join time |
| 16 | `internal/metrics`: host stats + guest report ingestion -> Prometheus + `get_vm_metrics` | row 10 merged, report JSON shape | ~50k | PR link |

Order: (12 ∥ 13) -> 14 -> (15 ∥ 16).

## Wave 4: CI and docs

| # | Deliverable | Inputs | Est. | Returns |
|---|---|---|---|---|
| 17 | GitHub Actions: unit/lint, image build in an archlinux container with caches and artifacts, KVM e2e job, nightly full run; bounded waits | repo, Makefile targets | ~60k | workflow run links |
| 18 | README as design doc (sibling structure), `docs/development.md` local loops, `vm-manager.service` unit and install guide | all merged work | ~40k | PR link |

Follow-ups outside the prototype: CAPI infrastructure provider / cluster-manager glue,
agent-platform wiring (muster MCPServer CR pointing at the host), tap/bridge network
backend, host-side pre-install fast path, EK-certified attestation keys, secure boot.
