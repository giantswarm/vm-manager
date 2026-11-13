<!-- BEGIN devctl-managed: edits in this block are overwritten -->
<!-- markdownlint-disable-file MD041 -->

## Giant Swarm conventions

devctl maintains this block from [its template][devctl-agents-block] and
overwrites changes to it. Instructions for this repository go in the section
below. Where they differ from this block, they take precedence.

[devctl-agents-block]: https://github.com/giantswarm/devctl/blob/main/pkg/gen/input/llm/internal/file/agents_block.md.template

- Never put customer names or other customer information in code, commit
  messages, issues or pull requests. Most Giant Swarm repositories are public,
  and private ones are kept ready to become public.
- Giant Swarm's style guides are in <https://github.com/giantswarm/fmt>, one
  directory per topic (go, helm, kubernetes, prometheus, …). Read the relevant
  guide before you write or review that kind of code.
- Use conventional commits for commit messages and pull request titles.
- If `.github/workflows/zz_generated.auto_release.yaml` exists, GitHub Releases
  are written from pull request titles: do not edit CHANGELOG.md. Otherwise,
  add your change to CHANGELOG.md as described in
  <https://github.com/giantswarm/fmt/blob/main/releases/changelog.md>.
- If `.pre-commit-config.yaml` exists, run
  `pre-commit run --files <changed files>` before committing; not every clone
  has the hooks installed.
- Do not add linters, formatters or other tooling unless asked; use what the
  repository already has.
- Put documentation in `docs/` rather than growing README.md, which gives an
  overview and links into `docs/`. When a change affects documented behaviour,
  update the documentation in the same pull request.
- If the skills of the `giantswarm/claude-code` plugin marketplace are not
  available to you and the user works at Giant Swarm, tell them once that
  they can install the `gs-base`, `gs-dev`, `gs-godev` and `gs-appdev`
  plugins from it.
- Go: run `goimports -local github.com/giantswarm/<repository>` on the files
  you change. CI with architect-orb fails when imports are grouped
  differently.

<!-- END devctl-managed -->

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
