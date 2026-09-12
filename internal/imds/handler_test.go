package imds

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

// httptest.NewRequest's default RemoteAddr; unknownAddr resolves to nothing.
const (
	knownAddr   = "192.0.2.1:1234"
	unknownAddr = "198.51.100.7:4"
	vmID        = "vm-0f3a"
	ignition    = `{"ignition":{"version":"3.4.0"}}`
)

// fakeResolver maps addresses to instances and lets tests flip release state.
type fakeResolver struct {
	mu        sync.Mutex
	instances map[netip.Addr]Instance
}

func (f *fakeResolver) LookupByIP(_ context.Context, ip netip.Addr) (Instance, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[ip]
	return inst, ok
}

func (f *fakeResolver) release(ip netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst := f.instances[ip]
	inst.UserDataReleased = true
	f.instances[ip] = inst
}

// fakeAttestor answers with a fixed nonce and a scripted verdict.
type fakeAttestor struct {
	nonce    string
	verified bool
	err      error
	got      QuoteRequest
}

func (f *fakeAttestor) Nonce(_ context.Context, _ string) (string, error) {
	return f.nonce, f.err
}

func (f *fakeAttestor) SubmitQuote(_ context.Context, _ string, req QuoteRequest) (QuoteResult, error) {
	f.got = req
	if f.err != nil {
		return QuoteResult{}, f.err
	}
	return QuoteResult{Verified: f.verified, Message: "scripted"}, nil
}

type fakeSink struct {
	vm  string
	raw json.RawMessage
	err error
}

func (f *fakeSink) StoreReport(_ context.Context, vmID string, raw json.RawMessage) error {
	f.vm, f.raw = vmID, raw
	return f.err
}

func testInstance() Instance {
	return Instance{
		ID:                vmID,
		Name:              "worker-1",
		Hostname:          "worker-1.example",
		Region:            "host-a",
		Zone:              "net-1",
		SSHAuthorizedKeys: []string{"ssh-ed25519 AAAA first", "ssh-ed25519 BBBB second"},
		KubernetesVersion: "1.33.2",
		Metadata:          map[string]string{"role": "worker", "empty": ""},
		UserData:          []byte(ignition),
	}
}

type fixture struct {
	handler  http.Handler
	resolver *fakeResolver
	attestor *fakeAttestor
	sink     *fakeSink
}

func newFixture(t *testing.T, inst Instance) *fixture {
	t.Helper()
	f := &fixture{
		resolver: &fakeResolver{instances: map[netip.Addr]Instance{netip.MustParseAddr("192.0.2.1"): inst}},
		attestor: &fakeAttestor{nonce: strings.Repeat("ab", 32), verified: true},
		sink:     &fakeSink{},
	}
	f.handler = Handler(Deps{
		Resolver: f.resolver,
		Attestor: f.attestor,
		Reports:  f.sink,
		Artifacts: FSArtifacts{FS: fstest.MapFS{
			"base/SHA256SUMS":                       {Data: []byte("deadbeef  giantswarm-vm-base_1.0.0.efi\n")},
			"base/SHA256SUMS.gpg":                   {Data: []byte{0x88, 0x01}},
			"base/giantswarm-vm-base_1.0.0.efi":     {Data: []byte("MZ-uki")},
			"kubernetes/SHA256SUMS":                 {Data: []byte("cafe  kubernetes_1.33.2.raw\n")},
			"kubernetes/kubernetes_1.33.2.raw":      {Data: []byte("sysext")},
			"kubernetes/nested/kubernetes_1.33.raw": {Data: []byte("hidden")},
		}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return f
}

func (f *fixture) do(t *testing.T, method, path, body, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, BasePath+path, strings.NewReader(body))
	if remote != "" {
		req.RemoteAddr = remote
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestHandlerPlainKeys(t *testing.T) {
	inst := testInstance()
	tests := []struct {
		name   string
		path   string
		inst   Instance
		status int
		body   string
		// muxNotFound marks a path outside the key table: the ServeMux
		// answers that 404 itself, with its own body.
		muxNotFound bool
	}{
		{name: "hostname", path: "/hostname", inst: inst, status: 200, body: "worker-1.example"},
		{name: "hostname falls back to name", path: "/hostname", inst: Instance{Name: "worker-1"}, status: 200, body: "worker-1"},
		{name: "region", path: "/region", inst: inst, status: 200, body: "host-a"},
		{name: "zone", path: "/zone", inst: inst, status: 200, body: "net-1"},
		{name: "zone unset", path: "/zone", inst: Instance{}, status: 404},
		{name: "instance-id", path: "/instance-id", inst: inst, status: 200, body: vmID},
		{name: "kubernetes-version", path: "/kubernetes-version", inst: inst, status: 200, body: "1.33.2"},
		{name: "kubernetes-version unset", path: "/kubernetes-version", inst: Instance{}, status: 404},
		{name: "public key index", path: "/public-keys/", inst: inst, status: 200, body: "0\n1"},
		{name: "public key index empty", path: "/public-keys/", inst: Instance{}, status: 200, body: ""},
		{name: "public key 0", path: "/public-keys/0", inst: inst, status: 200, body: "ssh-ed25519 AAAA first"},
		{name: "public key 1", path: "/public-keys/1", inst: inst, status: 200, body: "ssh-ed25519 BBBB second"},
		{name: "public key out of range", path: "/public-keys/2", inst: inst, status: 404},
		{name: "public key not a number", path: "/public-keys/first", inst: inst, status: 404},
		{name: "metadata", path: "/metadata/role", inst: inst, status: 200, body: "worker"},
		{name: "metadata explicitly empty", path: "/metadata/empty", inst: inst, status: 200, body: ""},
		{name: "metadata missing", path: "/metadata/nope", inst: inst, status: 404},
		{name: "unknown key", path: "/nope", inst: inst, status: 404, muxNotFound: true},
		{name: "value with newline kept verbatim", path: "/metadata/multi", inst: Instance{Metadata: map[string]string{"multi": "a\nb\n"}}, status: 200, body: "a\nb\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newFixture(t, tc.inst).do(t, http.MethodGet, tc.path, "", "")
			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, textPlain, rec.Header().Get("Content-Type"))
			if tc.status == http.StatusOK {
				assert.Equal(t, tc.body, rec.Body.String())
				assert.Equal(t, len(tc.body), rec.Body.Len())
			} else if !tc.muxNotFound {
				// systemd-imdsd aborts a >= 300 response as soon as it
				// carries body bytes and never reaches its 404 handling.
				assert.Empty(t, rec.Body.String(), "a 404 must have no body")
				assert.Equal(t, "0", rec.Header().Get("Content-Length"))
			}
		})
	}
}

func TestHandlerUnknownClientIsForbidden(t *testing.T) {
	f := newFixture(t, testInstance())
	for _, path := range []string{"/hostname", "/user-data", "/attest/nonce", "/sysupdate/base/SHA256SUMS", "/nope"} {
		rec := f.do(t, http.MethodGet, path, "", unknownAddr)
		assert.Equal(t, http.StatusForbidden, rec.Code, path)
	}

	t.Run("x-forwarded-for is ignored", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, BasePath+"/hostname", nil)
		req.RemoteAddr = unknownAddr
		req.Header.Set("X-Forwarded-For", "192.0.2.1")
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("injected client address", func(t *testing.T) {
		h := Handler(Deps{
			Resolver: f.resolver, Attestor: f.attestor, Reports: f.sink, Artifacts: FSArtifacts{FS: fstest.MapFS{}},
			ClientAddr: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.1"), true },
		})
		req := httptest.NewRequest(http.MethodGet, BasePath+"/hostname", nil)
		req.RemoteAddr = unknownAddr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("mapped ipv4 resolves", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/hostname", "", "[::ffff:192.0.2.1]:5")
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	f := newFixture(t, testInstance())
	for path, method := range map[string]string{
		"/hostname":     http.MethodPost,
		"/user-data":    http.MethodPut,
		"/attest/nonce": http.MethodPost,
		"/attest/quote": http.MethodGet,
		"/report":       http.MethodGet,
	} {
		rec := f.do(t, method, path, "", "")
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method+" "+path)
	}
}

func TestHandlerUserDataGating(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		inst := testInstance()
		inst.UserData = nil
		rec := newFixture(t, inst).do(t, http.MethodGet, "/user-data", "", "")
		// Ignition accepts 204 and parses the empty body as "no config"
		// (ErrEmpty); a 404 would fail its fetch stage.
		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Empty(t, rec.Body.String(), "a 204 has no body")
	})

	t.Run("gated until released", func(t *testing.T) {
		f := newFixture(t, testInstance())
		rec := f.do(t, http.MethodGet, "/user-data", "", "")
		// Ignition retries every status >= 500 with backoff; 403 or 404
		// would end its fetch stage with an error.
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, userDataRetryAfter, rec.Header().Get("Retry-After"))
		assert.Equal(t, userDataGatedBody, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "\n")

		f.resolver.release(netip.MustParseAddr("192.0.2.1"))
		rec = f.do(t, http.MethodGet, "/user-data", "", "")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, ignition, rec.Body.String())
		assert.Equal(t, textPlain, rec.Header().Get("Content-Type"))
	})
}

func quoteJSON(t *testing.T, stage Stage, nonce string) string {
	t.Helper()
	body, err := json.Marshal(QuoteRequest{
		Stage: stage, Nonce: nonce, AKPub: []byte("ak"), Quote: []byte("q"), Signature: []byte("s"),
		PCRs: map[string]map[string]string{"sha256": {"11": "00ff"}},
	})
	require.NoError(t, err)
	return string(body)
}

func TestHandlerAttest(t *testing.T) {
	t.Run("nonce", func(t *testing.T) {
		f := newFixture(t, testInstance())
		rec := f.do(t, http.MethodGet, "/attest/nonce", "", "")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, f.attestor.nonce, rec.Body.String())

		f.attestor.err = errors.New("tpm down")
		rec = f.do(t, http.MethodGet, "/attest/nonce", "", "")
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	tests := []struct {
		name     string
		body     string
		verified bool
		err      error
		status   int
		released bool
	}{
		{name: "initrd verified releases user-data", body: quoteJSON(t, StageInitrd, "n1"), verified: true, status: 200, released: true},
		{name: "ready verified does not release", body: quoteJSON(t, StageReady, "n1"), verified: true, status: 200},
		{name: "rejected", body: quoteJSON(t, StageInitrd, "n1"), verified: false, status: 403},
		{name: "bad json", body: "{", status: 400},
		{name: "bad stage", body: quoteJSON(t, "boot", "n1"), status: 400},
		{name: "missing nonce", body: quoteJSON(t, StageInitrd, ""), status: 400},
		{name: "missing pcrs", body: `{"stage":"initrd","nonce":"n","ak_pub":"YQ==","quote":"YQ==","signature":"YQ=="}`, status: 400},
		{name: "attestor invalid", body: quoteJSON(t, StageInitrd, "n1"), err: apierr.ErrInvalid, status: 400},
		{name: "attestor failure", body: quoteJSON(t, StageInitrd, "n1"), err: errors.New("boom"), status: 500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, testInstance())
			f.attestor.verified, f.attestor.err = tc.verified, tc.err
			rec := f.do(t, http.MethodPost, "/attest/quote", tc.body, "")
			assert.Equal(t, tc.status, rec.Code)
			if tc.status != http.StatusOK && tc.status != http.StatusForbidden {
				return
			}
			assert.Equal(t, applicationJSON, rec.Header().Get("Content-Type"))
			var res QuoteResult
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
			assert.Equal(t, f.attestor.got.Stage, res.Stage)
			assert.Equal(t, tc.verified, res.Verified)
			assert.Equal(t, tc.released, res.UserDataReleased)
			assert.Equal(t, "n1", f.attestor.got.Nonce)
			assert.Equal(t, "00ff", f.attestor.got.PCRs["sha256"]["11"])
		})
	}
}

func TestHandlerReport(t *testing.T) {
	f := newFixture(t, testInstance())
	rec := f.do(t, http.MethodPost, "/report", `{"io.systemd.Manager.unitsByTypeTotal":{"service":42}}`, "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, vmID, f.sink.vm)
	assert.JSONEq(t, `{"io.systemd.Manager.unitsByTypeTotal":{"service":42}}`, string(f.sink.raw))

	rec = f.do(t, http.MethodPost, "/report", "not json", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	f.sink.err = errors.New("disk full")
	rec = f.do(t, http.MethodPost, "/report", `{}`, "")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestHandlerSysupdate(t *testing.T) {
	f := newFixture(t, testInstance())
	tests := []struct {
		path   string
		status int
		body   string
	}{
		{"/sysupdate/base/SHA256SUMS", 200, "deadbeef  giantswarm-vm-base_1.0.0.efi\n"},
		{"/sysupdate/base/SHA256SUMS.gpg", 200, "\x88\x01"},
		{"/sysupdate/base/giantswarm-vm-base_1.0.0.efi", 200, "MZ-uki"},
		{"/sysupdate/kubernetes/kubernetes_1.33.2.raw", 200, "sysext"},
		{"/sysupdate/kubernetes/kubernetes_9.9.9.raw", 404, ""},
		{"/sysupdate/kubernetes/", 404, ""},
		{"/sysupdate/kubernetes/nested", 404, ""},
		{"/sysupdate/kubernetes/nested/kubernetes_1.33.raw", 404, ""},
		{"/sysupdate/other/SHA256SUMS", 404, ""},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, tc.path, "", "")
			assert.Equal(t, tc.status, rec.Code)
			if tc.status == http.StatusOK {
				assert.Equal(t, tc.body, rec.Body.String())
				assert.Equal(t, octetStream, rec.Header().Get("Content-Type"))
				assert.Equal(t, len(tc.body), rec.Body.Len())
			}
		})
	}

	t.Run("traversal is cleaned by the mux", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/sysupdate/kubernetes/../base/SHA256SUMS", "", "")
		assert.NotEqual(t, http.StatusOK, rec.Code)
	})
}

func TestHandlerPanicsOnMissingDeps(t *testing.T) {
	assert.PanicsWithValue(t, "imds: Deps.Attestor is nil", func() {
		Handler(Deps{Resolver: &fakeResolver{}, Reports: &fakeSink{}, Artifacts: FSArtifacts{}})
	})
}

func TestNoopAttestor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	a := &NoopAttestor{Now: func() time.Time { return now }}
	ctx := context.Background()

	nonce, err := a.Nonce(ctx, vmID)
	require.NoError(t, err)
	assert.Len(t, nonce, 64)

	res, err := a.SubmitQuote(ctx, "other-vm", QuoteRequest{Nonce: nonce})
	require.NoError(t, err)
	assert.False(t, res.Verified, "nonce is bound to the VM it was issued to")

	res, err = a.SubmitQuote(ctx, vmID, QuoteRequest{Nonce: nonce})
	require.NoError(t, err)
	assert.True(t, res.Verified)

	res, err = a.SubmitQuote(ctx, vmID, QuoteRequest{Nonce: nonce})
	require.NoError(t, err)
	assert.False(t, res.Verified, "nonces are single-use")

	nonce, err = a.Nonce(ctx, vmID)
	require.NoError(t, err)
	now = now.Add(NonceTTL + time.Second)
	res, err = a.SubmitQuote(ctx, vmID, QuoteRequest{Nonce: nonce})
	require.NoError(t, err)
	assert.False(t, res.Verified, "nonces expire after NonceTTL")
	assert.Empty(t, a.nonces, "expired nonces are swept")

	for i := 0; i < maxNoncesPerVM+3; i++ {
		_, err = a.Nonce(ctx, vmID)
		require.NoError(t, err)
	}
	assert.Len(t, a.nonces[vmID], maxNoncesPerVM, "outstanding nonces per VM are capped")
}

func TestReleasesUserData(t *testing.T) {
	assert.True(t, ReleasesUserData(StageInitrd, true))
	assert.False(t, ReleasesUserData(StageReady, true))
	assert.False(t, ReleasesUserData(StageInitrd, false))
}
