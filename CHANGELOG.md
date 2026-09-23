# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- Chart: a ServiceMonitor for `GET /metrics`, gated on `serviceMonitor.enabled` (needs `metrics.enabled` too) with a `serviceMonitor.labels` map. The Giant Swarm observability platform routes a scrape to a Mimir tenant by `observability.giantswarm.io/tenant`, and the chart rendered no ServiceMonitor at all, so `vm_manager_*` and `vm_guest_*` reached no tenant on every installation.
- The guest image artifact `gsoci.azurecr.io/giantswarm/vm-manager-guest-image:<version>` is signed at its digest with cosign keyless under the release job's CircleCI OIDC identity (the architect orb's `cosign-sign-verify`, a Sigstore bundle stored as an OCI referrer), the same shape and identity as the container image and the chart, so one keyless attestor admits all three release artifacts.

### Fixed

- QEMU starts only once the VM's swtpm has answered `CMD_GET_CAPABILITY` on its control socket, not as soon as the socket file exists; a swtpm that does not answer within 5s fails the start with `vtpm not responding`. A run whose console shows the guest's TPM calls failing (the EFI stub's `Failed to measure data for event`, the kernel's `tpm tpm0: Operation Timed out`) records `vtpm not responding: the guest's TPM calls failed (console: …)` in `lastError` in front of the install or boot timeout. swtpm logs every command at level 2, so a command it never answered is the last one in `tpm/swtpm.log`; the previous run's log is kept as `tpm/swtpm.prev.log`. The control socket no longer carries swtpm's `terminate` option (the readiness probe would end swtpm); `--terminate` ends swtpm once QEMU closes the data channel instead.
- Guest image: `vm-kubernetes.service` succeeds on a VM created without a Kubernetes version. It failed on every such VM: `/usr/lib/vm-manager/kubernetes` looked for the varlink error name `KeyNotFound`, which `systemd-imds` prints as `Key not available.`, and it runs `grep`, which reached the image only as a transitive dependency the Arch repositories dropped. `grep` is now an explicit package, the script matches what `systemd-imds` prints, and `TestNetworkIMDS` boots its VM from an image offering no Kubernetes version and asserts the unit succeeds.
- Chart: the `helm.sh/chart` label is a valid label value for any chart version. The 63-character cut of a long development version could end in `.`, `_` or `-`, and the API server refused the ServiceAccount, Service and Deployment; every non-alphanumeric character at the ends of the cut is now trimmed, and the render assertions check the label for such versions.
- The released image reported `version=dev`: the version, commit and build time are now resolved from the Go build info (the tag at HEAD, `vcs.revision`, `vcs.time`) when the build passed no `-ldflags -X`; the start-up log names the commit next to the version.

### Added

- `get_info` MCP tool reporting the server's version, commit, build time and tool names, as the other Agent Platform managers do.



[Unreleased]: https://github.com/giantswarm/vm-manager/tree/main
