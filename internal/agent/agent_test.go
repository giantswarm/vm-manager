package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/imds"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const testNonce = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// anyVM resolves every client address to one instance, as the in-stack
// listener would for the VM behind it.
type anyVM struct{}

func (anyVM) LookupByIP(context.Context, netip.Addr) (imds.Instance, bool) {
	return imds.Instance{ID: "vm-1", Name: "vm-1"}, true
}

type noVM struct{}

func (noVM) LookupByIP(context.Context, netip.Addr) (imds.Instance, bool) {
	return imds.Instance{}, false
}

// recordingAttestor hands out a fixed nonce and records the quote it gets.
type recordingAttestor struct {
	verified bool
	message  string
	err      error
	got      atomic.Pointer[imds.QuoteRequest]
}

func (a *recordingAttestor) Nonce(context.Context, string) (string, error) { return testNonce, a.err }

func (a *recordingAttestor) SubmitQuote(_ context.Context, _ string, req imds.QuoteRequest) (imds.QuoteResult, error) {
	a.got.Store(&req)
	return imds.QuoteResult{Verified: a.verified, Message: a.message}, a.err
}

type noReports struct{}

func (noReports) StoreReport(context.Context, string, json.RawMessage) error { return nil }

type noArtifacts struct{}

func (noArtifacts) Open(string, string) (fs.File, error) { return nil, fs.ErrNotExist }

// newIMDS serves the real imds handler so the agent is tested against the
// wire contract, not a re-implementation of it.
func newIMDS(t *testing.T, resolver imds.Resolver, attestor imds.Attestor) string {
	t.Helper()
	srv := httptest.NewServer(imds.Handler(imds.Deps{
		Resolver: resolver, Attestor: attestor, Reports: noReports{}, Artifacts: noArtifacts{}, Log: quiet,
	}))
	t.Cleanup(srv.Close)
	return srv.URL + imds.BasePath
}

// fakeQuote returns a well-formed request without a TPM.
func fakeQuote(stage imds.Stage, nonce string) (imds.QuoteRequest, error) {
	return imds.QuoteRequest{
		Stage: stage, Nonce: nonce,
		AKPub: []byte("ak"), EKPub: []byte("ek"), Quote: []byte("quoted"), Signature: []byte("sig"),
		PCRs:     map[string]map[string]string{"sha256": {"0": "00", "11": "0b"}},
		EventLog: []byte("log"),
	}, nil
}

func TestAttestVerifiedThroughTheRealIMDS(t *testing.T) {
	attestor := &imds.NoopAttestor{}
	url := newIMDS(t, anyVM{}, attestor)

	res, err := Attest(context.Background(), Config{
		IMDSURL: url, Stage: imds.StageInitrd, Quote: fakeQuote, Logger: quiet, UserAgent: "vm-agent/test",
	})
	require.NoError(t, err)
	assert.True(t, res.Verified)
	assert.True(t, res.UserDataReleased)
	assert.Equal(t, imds.StageInitrd, res.Stage)

	// The nonce came from the IMDS and was consumed: a second run with the
	// same nonce must be rejected, which also exercises ErrRejected.
	res, err = Attest(context.Background(), Config{
		IMDSURL: url, Stage: imds.StageReady, Quote: fakeQuote, Logger: quiet, Nonce: testNonce,
	})
	assert.ErrorIs(t, err, ErrRejected)
	assert.ErrorContains(t, err, "unknown or expired nonce")
	assert.False(t, res.Verified)
	assert.Equal(t, imds.StageReady, res.Stage)
}

func TestAttestJSONShape(t *testing.T) {
	attestor := &recordingAttestor{verified: true}
	url := newIMDS(t, anyVM{}, attestor)

	_, err := Attest(context.Background(), Config{IMDSURL: url, Stage: imds.StageReady, Quote: fakeQuote, Logger: quiet})
	require.NoError(t, err)

	got := attestor.got.Load()
	require.NotNil(t, got)
	want, _ := fakeQuote(imds.StageReady, testNonce)
	assert.Equal(t, want, *got, "the handler decodes exactly what the agent encoded")
}

func TestAttestOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		resolver imds.Resolver
		attestor *recordingAttestor
		quote    QuoteFunc
		stage    imds.Stage
		wantErr  string
		rejected bool
	}{
		{
			name: "rejected", resolver: anyVM{}, stage: imds.StageInitrd, quote: fakeQuote,
			attestor: &recordingAttestor{message: "pcr 11 mismatch"}, wantErr: "pcr 11 mismatch", rejected: true,
		},
		{
			name: "unknown vm", resolver: noVM{}, stage: imds.StageInitrd, quote: fakeQuote,
			attestor: &recordingAttestor{verified: true}, wantErr: "fetch nonce: HTTP 403: forbidden",
		},
		{
			name: "attestor down", resolver: anyVM{}, stage: imds.StageInitrd, quote: fakeQuote,
			attestor: &recordingAttestor{err: errors.New("boom")}, wantErr: "fetch nonce: HTTP 500: attestation unavailable",
		},
		{
			name: "quote fails", resolver: anyVM{}, stage: imds.StageInitrd, attestor: &recordingAttestor{verified: true},
			quote:   func(imds.Stage, string) (imds.QuoteRequest, error) { return imds.QuoteRequest{}, errors.New("no tpm") },
			wantErr: "build quote: no tpm",
		},
		{
			name: "malformed quote", resolver: anyVM{}, stage: imds.StageInitrd, attestor: &recordingAttestor{verified: true},
			quote: func(stage imds.Stage, nonce string) (imds.QuoteRequest, error) {
				return imds.QuoteRequest{Stage: stage, Nonce: nonce}, nil
			},
			wantErr: "post quote: HTTP 400",
		},
		{
			name: "bad stage", resolver: anyVM{}, stage: "boot", quote: fakeQuote,
			attestor: &recordingAttestor{verified: true}, wantErr: `stage must be "initrd" or "ready"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url := newIMDS(t, tc.resolver, tc.attestor)
			_, err := Attest(context.Background(), Config{IMDSURL: url, Stage: tc.stage, Quote: tc.quote, Logger: quiet})
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
			assert.Equal(t, tc.rejected, errors.Is(err, ErrRejected))
		})
	}
}

func TestAttestPrintDoesNotPost(t *testing.T) {
	attestor := &recordingAttestor{verified: true}
	url := newIMDS(t, anyVM{}, attestor)

	var out bytes.Buffer
	res, err := Attest(context.Background(), Config{
		IMDSURL: url, Stage: imds.StageInitrd, Quote: fakeQuote, Logger: quiet, Print: &out,
	})
	require.NoError(t, err)
	assert.Equal(t, imds.StageInitrd, res.Stage)
	assert.Nil(t, attestor.got.Load(), "nothing was posted")

	var printed imds.QuoteRequest
	require.NoError(t, json.Unmarshal(out.Bytes(), &printed))
	want, _ := fakeQuote(imds.StageInitrd, testNonce)
	assert.Equal(t, want, printed)
	assert.Contains(t, out.String(), `"ak_pub": "YWs="`, "binary fields are base64")
}

func TestAttestRetriesUntilTheIMDSIsUp(t *testing.T) {
	// Reserve an address, start the IMDS on it only after the agent has
	// failed a few times.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	srv := httptest.NewUnstartedServer(imds.Handler(imds.Deps{
		Resolver: anyVM{}, Attestor: &imds.NoopAttestor{}, Reports: noReports{}, Artifacts: noArtifacts{}, Log: quiet,
	}))
	started := make(chan struct{})
	t.Cleanup(func() {
		<-started
		srv.Close()
	})
	go func() {
		defer close(started)
		time.Sleep(300 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		_ = srv.Listener.Close()
		srv.Listener = l
		srv.Start()
	}()

	start := time.Now()
	res, err := Attest(context.Background(), Config{
		IMDSURL: "http://" + addr + imds.BasePath, Stage: imds.StageInitrd, Quote: fakeQuote, Logger: quiet,
		Backoff: 20 * time.Millisecond, Timeout: 10 * time.Second,
	})
	require.NoError(t, err)
	assert.True(t, res.Verified)
	assert.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond, "waited for the IMDS")
}

func TestAttestGivesUpAtTheTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	_, err = Attest(context.Background(), Config{
		IMDSURL: "http://" + addr, Stage: imds.StageInitrd, Quote: fakeQuote, Logger: quiet,
		Backoff: 10 * time.Millisecond, Timeout: 200 * time.Millisecond,
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "imds not reachable within the timeout")
	assert.False(t, errors.Is(err, ErrRejected))
}

func TestAttestRequiresQuoteFunc(t *testing.T) {
	_, err := Attest(context.Background(), Config{Stage: imds.StageInitrd})
	assert.ErrorContains(t, err, "no quote function")
}

func TestDescribe(t *testing.T) {
	assert.Equal(t, "HTTP 500", describe(http.StatusInternalServerError, nil))
	assert.Equal(t, "HTTP 403: the IMDS does not map this address to a VM", describe(http.StatusForbidden, []byte(" \n")))
	assert.Equal(t, "HTTP 400: bad", describe(http.StatusBadRequest, []byte("bad\n")))
	long := describe(http.StatusBadRequest, []byte(strings.Repeat("x", 2*maxErrorBody)))
	assert.Len(t, long, len("HTTP 400: ")+maxErrorBody)
}
