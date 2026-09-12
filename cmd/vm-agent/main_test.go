package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/agent/quote"
	"github.com/giantswarm/vm-manager/internal/imds"
)

type anyVM struct{}

func (anyVM) LookupByIP(context.Context, netip.Addr) (imds.Instance, bool) {
	return imds.Instance{ID: "vm-1"}, true
}

type verdict struct{ verified bool }

func (verdict) Nonce(context.Context, string) (string, error) { return "0badf00d", nil }

func (v verdict) SubmitQuote(context.Context, string, imds.QuoteRequest) (imds.QuoteResult, error) {
	return imds.QuoteResult{Verified: v.verified, Message: "policy says " + map[bool]string{true: "yes", false: "no"}[v.verified]}, nil
}

type noReports struct{}

func (noReports) StoreReport(context.Context, string, json.RawMessage) error { return nil }

type noArtifacts struct{}

func (noArtifacts) Open(string, string) (fs.File, error) { return nil, fs.ErrNotExist }

func newIMDS(t *testing.T, verified bool) string {
	t.Helper()
	srv := httptest.NewServer(imds.Handler(imds.Deps{
		Resolver: anyVM{}, Attestor: verdict{verified}, Reports: noReports{}, Artifacts: noArtifacts{},
	}))
	t.Cleanup(srv.Close)
	return srv.URL + imds.BasePath
}

// fakeBuild stands in for quote.Build and records the options it got.
func fakeBuild(got *quote.Options) buildFunc {
	return func(opts quote.Options, stage imds.Stage, nonce string) (imds.QuoteRequest, error) {
		*got = opts
		return imds.QuoteRequest{
			Stage: stage, Nonce: nonce, AKPub: []byte("ak"), Quote: []byte("q"), Signature: []byte("s"),
			PCRs: map[string]map[string]string{"sha256": {"11": "0b"}},
		}, nil
	}
}

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		verified bool
		args     []string
		build    buildFunc
		code     int
		stdout   string
		stderr   string
	}{
		{name: "verified", verified: true, args: []string{"attest", "--stage=initrd"}, code: exitOK},
		{name: "rejected", verified: false, args: []string{"attest", "--stage=ready"}, code: exitRejected, stderr: "Attestation rejected: quote rejected by the verifier: policy says no"},
		{name: "print", verified: false, args: []string{"attest", "--stage=ready", "--print"}, code: exitOK, stdout: `"stage": "ready"`},
		{name: "print with nonce", verified: false, args: []string{"attest", "--stage=ready", "--print", "--nonce=cafe"}, code: exitOK, stdout: `"nonce": "cafe"`},
		{name: "missing stage", args: []string{"attest"}, code: exitError, stderr: `required flag(s) "stage" not set`},
		{name: "bad stage", args: []string{"attest", "--stage=boot"}, code: exitError, stderr: `Error: stage must be "initrd" or "ready"`},
		{
			name: "quote error", args: []string{"attest", "--stage=initrd"}, code: exitError, stderr: "Error: build quote: no tpm",
			build: func(quote.Options, imds.Stage, string) (imds.QuoteRequest, error) {
				return imds.QuoteRequest{}, errors.New("no tpm")
			},
		},
		{name: "version", args: []string{"version"}, code: exitOK, stdout: "vm-agent version dev"},
		{name: "unknown command", args: []string{"nope"}, code: exitError, stderr: `unknown command "nope"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url := newIMDS(t, tc.verified)
			var got quote.Options
			build := tc.build
			if build == nil {
				build = fakeBuild(&got)
			}
			var stdout, stderr bytes.Buffer
			args := append([]string(nil), tc.args...)
			if args[0] == "attest" {
				args = append(args, "--imds-url="+url, "--tpm=/dev/test-tpm")
			}
			code := run(context.Background(), args, &stdout, &stderr, build)
			assert.Equal(t, tc.code, code, "stdout=%q stderr=%q", stdout.String(), stderr.String())
			assert.Contains(t, stdout.String(), tc.stdout)
			assert.Contains(t, stderr.String(), tc.stderr)
			if tc.build == nil && args[0] == "attest" && tc.code != exitError {
				assert.Equal(t, "/dev/test-tpm", got.Device, "the --tpm flag reaches quote.Build")
			}
		})
	}
}

func TestRunPrintIsValidJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var got quote.Options
	code := run(context.Background(), []string{"attest", "--stage=initrd", "--print", "--nonce=0102", "--imds-url=http://127.0.0.1:1"}, &stdout, &stderr, fakeBuild(&got))
	require.Equal(t, exitOK, code, stderr.String())
	var req imds.QuoteRequest
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &req))
	assert.Equal(t, imds.StageInitrd, req.Stage)
	assert.Equal(t, "0102", req.Nonce)
}

func TestRunPCRsWithoutDevice(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"pcrs", "--tpm=" + filepath.Join(t.TempDir(), "none")}, &stdout, &stderr, quote.Build)
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), "Error: open tpm")

	code = run(context.Background(), []string{"pcrs", "--bank=md5"}, &stdout, &stderr, quote.Build)
	assert.Equal(t, exitError, code)
	assert.Contains(t, stderr.String(), `unknown pcr bank "md5"`)
}

func TestRunHonoursContext(t *testing.T) {
	// A cancelled context (systemd stopping the unit) ends the retry loop
	// with an error rather than hanging until --timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var got quote.Options
	code := run(ctx, []string{"attest", "--stage=initrd", "--imds-url=http://127.0.0.1:1", "--timeout=30s"}, io.Discard, io.Discard, fakeBuild(&got))
	assert.Equal(t, exitError, code)
}
