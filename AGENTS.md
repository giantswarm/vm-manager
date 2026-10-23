# AGENTS.md

How vm-manager is built and tested: [docs/development.md](docs/development.md). The decided
architecture: [docs/design.md](docs/design.md).

## Design

- systemd's own primitives come first: systemd-sysinstall, the storage provider, systemd-firstboot,
  imdsd, sysupdate, sysext, measure and pcrlock, the `io.systemd.Metrics` varlink interface. Check
  the primitive's man page before proposing code; Ignition is the chosen executor for CAPI user-data.
- Go code only where systemd has no primitive (the TPM quote in the guest, its verification on the
  host), and the change says so.

## Tests

- A flaky test is a race until proven otherwise: fix it, do not rerun it green.
- An agent runs builds, image builds and the end-to-end suite in the foreground with a timeout of up
  to ten minutes each: a session that sends them to the background ends its turn and stalls.
