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

# image targets live in images/Makefile
# (make image / make e2e are wired here once images/ lands: the mkosi build,
# image-verify and the KVM boot e2e from docs/design.md "Testing strategy".)
