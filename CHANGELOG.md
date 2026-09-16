# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Fixed

- The released image reported `version=dev`: the version, commit and build time are now resolved from the Go build info (the tag at HEAD, `vcs.revision`, `vcs.time`) when the build passed no `-ldflags -X`; the start-up log names the commit next to the version.

### Added

- `get_info` MCP tool reporting the server's version, commit, build time and tool names, as the other Agent Platform managers do.



[Unreleased]: https://github.com/giantswarm/vm-manager/tree/main
