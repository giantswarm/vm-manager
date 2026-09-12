package cmd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/attest"
	"github.com/giantswarm/vm-manager/internal/imds"
	"github.com/giantswarm/vm-manager/internal/metrics"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// TestServeWiring runs serve the way CI does: a fresh state dir, no images,
// no /dev/kvm and possibly no AF_VSOCK. Every component must come up, the
// default network must exist, and cancelling the context must shut it all
// down within a few seconds.
func TestServeWiring(t *testing.T) {
	// Unix socket paths below the state dir are length-limited; keep it short.
	stateDir, err := os.MkdirTemp("", "vmm-serve")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	o := &serveOptions{
		listen:                  addr,
		mcpPath:                 "/mcp",
		stateDir:                stateDir,
		networkSubnet:           "192.168.221.0/24",
		defaultNetwork:          "default",
		installTimeout:          vm.DefaultInstallTimeout,
		bootTimeout:             vm.DefaultBootTimeout,
		stopTimeout:             2 * time.Second,
		metricsEnabled:          true,
		metricsGuestSeriesLimit: metrics.DefaultMaxGuestSeries,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- runServe(ctx, o) }()

	var nets []vm.NetworkInfo
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + api.Prefix + "/networks")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK && json.Unmarshal(body, &nets) == nil && len(nets) == 1
	}, 5*time.Second, 50*time.Millisecond, "serve did not come up")
	assert.Less(t, time.Since(start), 5*time.Second, "startup")
	assert.Equal(t, "default", nets[0].Spec.Name)
	assert.Equal(t, "192.168.221.0/24", nets[0].Spec.CIDR)
	assert.Equal(t, "192.168.221.1", nets[0].Gateway)
	assert.True(t, nets[0].Spec.EnableIMDS, "the default network serves the IMDS")

	resp, err := http.Get("http://" + addr + api.Prefix + "/images")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, "[]", string(body), "an empty image dir is a warning, not an error")
	assert.DirExists(t, filepath.Join(stateDir, imagesSubdir), "the default image dir is created")

	resp, err = http.Get("http://" + addr + "/metrics")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `vm_manager_network_leases{network="default"} 0`, "the host collector scrapes the VM service")
	assert.Contains(t, string(body), `vm_manager_vms{state="ready"} 0`)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop within 5s")
	}

	// A second start restores the network from networks.json instead of
	// creating it again.
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- runServe(ctx, o) }()
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + api.Prefix + "/networks/default")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 50*time.Millisecond, "serve did not come up again")
	cancel()
	require.NoError(t, <-done)
}

// TestNewComponentsFailureCleansUp makes the image catalog fail to load: the
// components built before it are closed and the error is returned, not a
// nil dereference in the deferred close.
func TestNewComponentsFailureCleansUp(t *testing.T) {
	stateDir, err := os.MkdirTemp("", "vmm-fail")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	notADir := filepath.Join(stateDir, "images-file")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o600))
	o := &serveOptions{stateDir: stateDir, imageDir: notADir, networkSubnet: "192.168.222.0/24", defaultNetwork: "default", stopTimeout: time.Second}

	c, err := newComponents(context.Background(), o, metrics.New(metrics.Options{}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.ErrorContains(t, err, "load images")
	assert.Nil(t, c)
}

func TestServeOptionsComplete(t *testing.T) {
	base := func() *serveOptions {
		return &serveOptions{stateDir: t.TempDir(), networkSubnet: "10.0.0.0/24", defaultNetwork: "default"}
	}
	o := base()
	require.NoError(t, o.complete())
	assert.Equal(t, filepath.Join(o.stateDir, imagesSubdir), o.imageDir)

	o = base()
	o.networkSubnet = "nope"
	assert.ErrorContains(t, o.complete(), "--network-subnet")

	o = base()
	o.notifyPort = -1
	assert.ErrorContains(t, o.complete(), "--notify-port")

	o = base()
	o.defaultNetwork = ""
	assert.ErrorContains(t, o.complete(), "--default-network")

	o = base()
	require.NoError(t, o.complete())
	assert.Equal(t, metrics.DefaultMaxGuestSeries, o.metricsGuestSeriesLimit, "an unset limit is the default")
	assert.Equal(t, attestationVerify, o.attestation, "empty means the default, verify")
	o = base()
	o.metricsGuestSeriesLimit = -1
	assert.ErrorContains(t, o.complete(), "--metrics-guest-series-limit")

	o = base()
	o.attestation = "maybe"
	assert.ErrorContains(t, o.complete(), "--attestation: \"maybe\"")

	o = base()
	o.attestation, o.learnGolden = attestationNoop, true
	assert.ErrorContains(t, o.complete(), "--attestation-learn-golden needs --attestation=verify")

	o = base()
	o.attestation, o.learnGolden = attestationVerify, true
	require.NoError(t, o.complete())
}

// TestAttestorWiring builds both attestors; the verifier resolves the VM
// service lazily, so it must not need c.vm at construction.
func TestAttestorWiring(t *testing.T) {
	c := &components{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := c.attestor(&serveOptions{attestation: attestationNoop}, log)
	require.NoError(t, err)
	assert.IsType(t, &imds.NoopAttestor{}, a)

	a, err = c.attestor(&serveOptions{attestation: attestationVerify, learnGolden: true}, log)
	require.NoError(t, err)
	assert.IsType(t, &attest.Verifier{}, a)
	_, err = a.SubmitQuote(context.Background(), "vm-1", imds.QuoteRequest{Stage: imds.StageInitrd, Nonce: "00"})
	assert.NoError(t, err, "an unknown nonce is a rejection, not an error, and the policy is never consulted")
}
