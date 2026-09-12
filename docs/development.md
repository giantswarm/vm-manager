# Developing on vm-manager

```sh
make build          # binary for the current platform (./vm-manager)
make test           # unit + contract tests, race detector when a C toolchain is present
make test-race      # the same with -race forced
make lint           # golangci-lint v2 with the pre-commit linters (gosec, goconst, govet)
make serve          # go run . -v serve (SERVE_ARGS="--listen 127.0.0.1:18080" to override)
make help           # every target with its description
```

`make image` and `make e2e` (mkosi build, image-verify, KVM boot tests) arrive
with `images/`; see `Makefile.custom.mk` for the placeholder and
[design.md](design.md) "Testing strategy" for the tiers.

## Layout

- `cmd/` — cobra CLI (`serve`, `version`). Every `serve` flag has an
  environment fallback named in its help text (`VM_MANAGER_*`, plus the
  sibling-compatible `DEX_*` / `GOOGLE_*` / `OAUTH_TRUSTED_AUDIENCES`).
- `internal/host` — the host capability service: kernel, CPUs, memory,
  `/dev/kvm` and `/dev/vhost-vsock` access, qemu / swtpm / systemd versions,
  the OVMF code image, the `io.systemd.StorageProvider` sockets, and the
  `ready` / `missing` verdict. Commands go through an injectable `Runner`
  and file probes are relative to `Options.Root`, so tests run on a fixture
  tree without the VM stack.
- `internal/apierr` — the sentinel errors (`ErrNotFound`, `ErrInvalid`,
  `ErrConflict`, `ErrUnsupported`) domain packages wrap so both API surfaces
  answer the same status and code.
- `internal/api` — REST handlers (`rest.go`) and MCP tools (`mcp.go`) over
  the same service methods, wired through `api.Services`. `statusFor` maps
  sentinels to an HTTP status and a stable code; MCP tool errors carry the
  same code. `mcp_test.go` is the contract test: an mcp-go client over
  streamable HTTP against the assembled server.
- `internal/server` — the single HTTP listener (`/healthz`, `/readyz`,
  `/api/v1`, `/mcp`); with `--enable-oauth` the mcp-oauth resource server
  (Dex or Google) in front of REST and MCP.
- `internal/identity` — the authenticated caller on the request context.
- `api/openapi.yaml` — the REST contract; served at `/api/v1/openapi.yaml`.

Domain packages from [design.md](design.md) (`internal/vm`,
`internal/runtime/qemu`, `internal/network`, `internal/imds`,
`internal/storage`, `internal/attest`, `internal/images`) plug in the same
way as `internal/host`: a `Service` with context-taking methods, a field on
`api.Services`, one `s.AddTool` in `NewMCPServer` and one route in
`REST.Register` per operation, errors wrapped from `internal/apierr`.

## Adding a tool

1. Add the `Tool<Name>` constant and list it in `ToolNames()`.
2. `s.AddTool(mcp.NewTool(Tool<Name>, mcp.WithDescription("Read-only. ..." or
   "WRITES: ..."), args..., annotations...), t.<handler>)`; the handler returns
   `jsonResult(v)` or `errResult(err), nil`.
3. Mirror it in `REST.Register` with a Go 1.22 mux pattern and document it in
   `api/openapi.yaml`.
4. Extend the contract test's expected tool list and the README table.

## Local loop

```sh
make build
./vm-manager serve -v --listen 127.0.0.1:18080

curl -s localhost:18080/healthz
curl -s localhost:18080/api/v1/host | jq          # ready, missing, versions
curl -s localhost:18080/api/v1/openapi.yaml
```

The same operation as an MCP tool call over streamable HTTP (initialize, then
`tools/call`; the session id from the initialize response goes into
`Mcp-Session-Id`):

```sh
MCP=localhost:18080/mcp
H='-H content-type:application/json -H accept:application/json,text/event-stream'
SID=$(curl -si $H -X POST $MCP -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}' | awk 'tolower($1)=="mcp-session-id:"{print $2}' | tr -d '\r')
curl -s $H -H "mcp-session-id: $SID" -X POST $MCP -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
curl -s $H -H "mcp-session-id: $SID" -X POST $MCP -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
curl -s $H -H "mcp-session-id: $SID" -X POST $MCP -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_host"}}'
```

With `mcp-debug` or any MCP client, point it at `http://127.0.0.1:18080/mcp`.

## OAuth against a lab Dex

```sh
./vm-manager serve -v --listen 127.0.0.1:18080 --enable-oauth \
  --oauth-base-url http://localhost:18080 \
  --dex-issuer-url https://dex.lab.example/dex --dex-client-id agent-platform --dex-client-secret ... \
  --oauth-trusted-audiences agent-platform --allow-private-oauth-urls --sso-allow-private-ips
```

`/healthz`, `/readyz` and the OAuth metadata stay public; `/api/v1` and `/mcp`
require a bearer token: an id_token for a trusted audience (what muster
forwards) or a token from this server's own OAuth flow.
