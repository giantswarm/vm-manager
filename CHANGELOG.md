# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- Chart: a ServiceMonitor for `GET /metrics`, gated on `serviceMonitor.enabled` (needs `metrics.enabled` too) with a `serviceMonitor.labels` map. The Giant Swarm observability platform routes a scrape to a Mimir tenant by `observability.giantswarm.io/tenant`, and the chart rendered no ServiceMonitor at all, so `vm_manager_*` and `vm_guest_*` reached no tenant on every installation.
- The guest image artifact `gsoci.azurecr.io/giantswarm/vm-manager-guest-image:<version>` is signed at its digest with cosign keyless under the release job's CircleCI OIDC identity (the architect orb's `cosign-sign-verify`, a Sigstore bundle stored as an OCI referrer), the same shape and identity as the container image and the chart, so one keyless attestor admits all three release artifacts.

### Fixed

- Guest image: `grep` is an explicit package of the base image. It used to arrive only as a transitive dependency, which the Arch repositories dropped, and `vm-kubernetes.service` needs it to recognise a VM created without a Kubernetes version; without it the unit failed on every such VM. `TestNetworkIMDS` now boots its VM from an image offering no Kubernetes version and asserts the unit succeeds.
- The released image reported `version=dev`: the version, commit and build time are now resolved from the Go build info (the tag at HEAD, `vcs.revision`, `vcs.time`) when the build passed no `-ldflags -X`; the start-up log names the commit next to the version.

### Added

- `get_info` MCP tool reporting the server's version, commit, build time and tool names, as the other Agent Platform managers do.



[Unreleased]: https://github.com/giantswarm/vm-manager/tree/main
