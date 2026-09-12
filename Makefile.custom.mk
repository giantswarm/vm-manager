##@ Development

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

##@ End-to-end

# T3 boot tests of docs/design.md "Testing strategy": real QEMU on KVM with
# swtpm, OVMF and the image from images/build (override the directory with
# VM_MANAGER_E2E_IMAGE_DIR). The tests skip, naming the reason, on a host
# without /dev/kvm, /dev/vhost-vsock, qemu, swtpm, OVMF or the artifacts;
# `make image` builds the latter. They print install_seconds= and
# boot_to_ready_seconds= for the boot-time budgets.

.PHONY: e2e
e2e: ## Run the KVM boot end-to-end tests (go test -tags e2e ./e2e/...).
	go test -tags e2e -count=1 -timeout 30m -v ./e2e/...
