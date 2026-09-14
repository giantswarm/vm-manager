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

# go vet ./... and golangci-lint ./... skip files behind the e2e build tag,
# which let the package break unnoticed twice. These two compile and lint it
# without a KVM host; .github/workflows/test.yml runs them on every PR.

.PHONY: vet-e2e
vet-e2e: ## Compile-check the e2e-tagged package without running it (go vet -tags e2e ./e2e/...).
	go vet -tags e2e ./e2e/...

.PHONY: lint-e2e
lint-e2e: ## golangci-lint of the e2e-tagged package with the linters of make lint.
	golangci-lint run -E gosec -E goconst --build-tags e2e --timeout=15m ./e2e/...

##@ Container image and chart

# The pod shape of vm-manager: the image (Dockerfile) and the chart
# (helm/vm-manager) the agent-platform meta chart installs as
# components.vm-manager. Released by the generated CircleCI pipeline
# (.circleci/workflows.yml: push-to-registries and push-to-app-catalog to
# gsoci and the giantswarm catalog) on every tag the Auto Release workflow
# cuts; the guest image artifact by the guest-image job of .circleci/custom.yml.

BINARY := vm-manager

.PHONY: build-linux-amd64
build-linux-amd64: ## Build the static linux/amd64 binary the Dockerfile expects (what CircleCI's go-build attaches).
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X '$(MODULE)/pkg/project.version=$(or $(VERSION),dev)' -X '$(MODULE)/pkg/project.buildTimestamp=$(BUILDTIMESTAMP)' -X '$(MODULE)/pkg/project.gitSHA=$(GITSHA1)'" -o $(BINARY)-linux-amd64 .

.PHONY: docker-build
docker-build: build-linux-amd64 ## Build the container image locally (TAG=vm-manager:dev): the binary and the QEMU/swtpm/OVMF runtime.
	docker build --build-arg TARGETOS=linux --build-arg TARGETARCH=amd64 -t $(or $(TAG),vm-manager:dev) .

.PHONY: guest-image
guest-image: ## Build the guest image in a privileged archlinux container (hack/guest-image-build.sh) into images/build/; `make image` is the native Arch build.
	hack/guest-image-build.sh

.PHONY: helm-lint
helm-lint: ## Lint the chart.
	helm lint helm/vm-manager

.PHONY: helm-template
helm-template: ## Render the chart with defaults.
	helm template vm-manager helm/vm-manager

.PHONY: helm-verify
helm-verify: ## Render assertions for the chart (hack/verify-chart.sh).
	hack/verify-chart.sh

# values.schema.json and the chart README are regenerated by the generated
# pre-commit hooks (helm-schema-vm-manager, helm-docs): `pre-commit run -a`.

.PHONY: helm-test
helm-test: helm-lint helm-verify ## Every offline chart check (what the chart workflow runs); the install smoke is the ATS job of the CircleCI pipeline.
