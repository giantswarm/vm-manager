##@ Development

# Makefile.gen.go.mk links everything with -extldflags -static on linux. The
# release binaries are CGO_ENABLED=0 and static regardless, but the cgo test
# binaries are not: internal/agent/quote tests against the go-tpm-tools
# simulator, which links OpenSSL, and a static link of that needs
# libcrypto.a, which most distributions do not ship. Let cgo test binaries
# link dynamically.
override EXTLDFLAGS :=

.PHONY: test-race
test-race: ## Run tests with the race detector regardless of the toolchain probe in Makefile.gen.go.mk.
	go test -race ./...

.PHONY: test-integration
test-integration: ## Run the integration-tagged tests: real QEMU, OVMF, swtpm and the host's systemd storage provider; each skips where its dependency is absent.
	go test -race -count=1 -tags integration -run Integration ./internal/...

.PHONY: serve
serve: ## Run the server locally with debug logging (make run prints the CLI help).
	go run . -v serve $(SERVE_ARGS)

##@ Images

# The mkosi project lives in images/ (see images/README.md); these are the
# entry points from the repo root.

.PHONY: image
image: ## Build the guest image with mkosi (dev keys + base image) into images/build/.
	$(MAKE) -C images keys base

.PHONY: image-verify
image-verify: ## Offline checks of the built image (GPT, UKI sections, expected PCR 11, hwdb, presets).
	$(MAKE) -C images verify

##@ Guest agent

# cmd/vm-agent is copied into the initrd and the root file system of the
# guest image, so it must be fully static: no cgo, no libc. The target
# asserts that with file(1) and ldd(1) after linking. VERSION comes from
# Makefile.gen.go.mk (gitsemver) and falls back to dev without it.
AGENT_VERSION ?= $(or $(VERSION),dev)

.PHONY: agent
agent: ## Build the static guest binary bin/vm-agent (attest, pcrs, version).
	@echo "====> $@"
	CGO_ENABLED=0 GOOS=linux go build -trimpath \
		-ldflags='-s -w -X main.version=$(AGENT_VERSION) -X main.commit=$(GITSHA1) -X main.date=$(BUILDTIMESTAMP)' \
		-o bin/vm-agent ./cmd/vm-agent
	@file bin/vm-agent | grep -q 'statically linked' || { echo "bin/vm-agent is not statically linked:"; file bin/vm-agent; exit 1; }
	@if ldd bin/vm-agent >/dev/null 2>&1; then echo "bin/vm-agent has dynamic dependencies:"; ldd bin/vm-agent; exit 1; fi
	@ls -l bin/vm-agent

##@ End-to-end

# T3 boot tests of docs/design.md "Testing strategy": real QEMU on KVM with
# swtpm, OVMF and the image from images/build (override the directory with
# VM_MANAGER_E2E_IMAGE_DIR). The tests skip, naming the reason, on a host
# without /dev/kvm, /dev/vhost-vsock, qemu, swtpm, OVMF or the artifacts;
# `make image` builds the latter. They print install_seconds= and
# boot_to_ready_seconds= for the boot-time budgets.

.PHONY: e2e
e2e: ## Run the KVM boot end-to-end tests (go test -tags e2e ./e2e/...).
	go test -tags e2e -count=1 -timeout 45m -v ./e2e/...
