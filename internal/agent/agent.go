// Package agent runs the guest side of the attestation protocol
// (docs/design.md "Attestation protocol"): fetch a nonce from the IMDS,
// have a quote built over it and post the quote. The TPM work is behind
// QuoteFunc so this package and its tests need no TPM; cmd/vm-agent wires
// quote.Build in.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/vm-manager/internal/agent/quote"
	"github.com/giantswarm/vm-manager/internal/imds"
)

// DefaultIMDSURL is where the guest reaches vm-manager's IMDS.
const DefaultIMDSURL = imds.DataURL

// Defaults for Config.
const (
	// DefaultTimeout bounds the whole run, including the wait for the IMDS
	// to come up.
	DefaultTimeout = 90 * time.Second
	// DefaultBackoff is the first retry delay; it doubles up to maxBackoff.
	DefaultBackoff = 250 * time.Millisecond
	maxBackoff     = 5 * time.Second
	// maxNonceBody bounds the /attest/nonce answer (32 hex bytes).
	maxNonceBody = 1 << 10
	// maxErrorBody bounds what of an error answer ends up in the message.
	maxErrorBody = 512
)

// ErrRejected is returned when the verifier answered the quote but did not
// accept it. The QuoteResult returned alongside carries its message.
var ErrRejected = errors.New("quote rejected by the verifier")

// QuoteFunc builds the request for stage over nonce (quote.Build bound to
// its options).
type QuoteFunc func(stage imds.Stage, nonce string) (imds.QuoteRequest, error)

// Config configures one Attest run.
type Config struct {
	// IMDSURL is the IMDS base URL; empty means DefaultIMDSURL.
	IMDSURL string
	// Stage is the boot phase being quoted.
	Stage imds.Stage
	// Nonce, when set, is used instead of fetching one from the IMDS.
	Nonce string
	// Timeout bounds the run; 0 means DefaultTimeout.
	Timeout time.Duration
	// Print, when set, receives the request as indented JSON instead of it
	// being posted.
	Print io.Writer
	// Quote builds the request; required.
	Quote QuoteFunc
	// Client is the HTTP client; nil uses http.DefaultClient.
	Client *http.Client
	// UserAgent is sent with every request when set.
	UserAgent string
	// Backoff is the first retry delay; 0 means DefaultBackoff.
	Backoff time.Duration
	// Logger is for progress; nil means slog.Default().
	Logger *slog.Logger
}

// Attest runs the protocol once. It returns the verifier's answer; a nil
// error means Verified is true. ErrRejected (with the result filled) means
// the verifier turned the quote down; other errors mean it never judged one.
// Requests are retried with backoff while the IMDS is not reachable, until
// Timeout.
func Attest(ctx context.Context, cfg Config) (imds.QuoteResult, error) {
	if cfg.Quote == nil {
		return imds.QuoteResult{}, errors.New("no quote function configured")
	}
	if !cfg.Stage.Valid() {
		return imds.QuoteResult{}, fmt.Errorf("stage must be %q or %q", imds.StageInitrd, imds.StageReady)
	}
	a := newAttester(cfg)
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	nonce := cfg.Nonce
	if nonce == "" {
		var err error
		if nonce, err = a.fetchNonce(ctx); err != nil {
			return imds.QuoteResult{}, err
		}
	}
	a.log.DebugContext(ctx, "building quote", "stage", cfg.Stage)
	req, err := cfg.Quote(cfg.Stage, nonce)
	if err != nil {
		return imds.QuoteResult{}, fmt.Errorf("build quote: %w", err)
	}
	if cfg.Print != nil {
		return imds.QuoteResult{Stage: cfg.Stage}, print(cfg.Print, req)
	}
	return a.postQuote(ctx, req)
}

type attester struct {
	base      string
	client    *http.Client
	userAgent string
	timeout   time.Duration
	backoff   time.Duration
	log       *slog.Logger
}

func newAttester(cfg Config) *attester {
	a := &attester{
		base:      strings.TrimSuffix(cfg.IMDSURL, "/"),
		client:    cfg.Client,
		userAgent: cfg.UserAgent,
		timeout:   cfg.Timeout,
		backoff:   cfg.Backoff,
		log:       cfg.Logger,
	}
	if a.base == "" {
		a.base = DefaultIMDSURL
	}
	if a.client == nil {
		a.client = http.DefaultClient
	}
	if a.timeout <= 0 {
		a.timeout = DefaultTimeout
	}
	if a.backoff <= 0 {
		a.backoff = DefaultBackoff
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	return a
}

// fetchNonce GETs /attest/nonce and checks the answer is hex.
func (a *attester) fetchNonce(ctx context.Context) (string, error) {
	resp, err := a.do(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/attest/nonce", nil)
	})
	if err != nil {
		return "", fmt.Errorf("fetch nonce: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNonceBody))
	if err != nil {
		return "", fmt.Errorf("fetch nonce: read answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch nonce: %s", describe(resp.StatusCode, body))
	}
	nonce := strings.TrimSpace(string(body))
	if _, err := quote.DecodeNonce(nonce); err != nil {
		return "", fmt.Errorf("fetch nonce: %w", err)
	}
	a.log.DebugContext(ctx, "nonce received", "bytes", len(nonce)/2)
	return nonce, nil
}

// postQuote POSTs the request to /attest/quote and decodes the verdict. The
// IMDS answers 200 with a verified result and 403 with a rejected one; a
// 403 that is not JSON means the IMDS does not know the caller at all.
func (a *attester) postQuote(ctx context.Context, req imds.QuoteRequest) (imds.QuoteResult, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return imds.QuoteResult{}, fmt.Errorf("encode quote: %w", err)
	}
	resp, err := a.do(ctx, func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/attest/quote", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		return r, nil
	})
	if err != nil {
		return imds.QuoteResult{}, fmt.Errorf("post quote: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !isJSON(resp) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return imds.QuoteResult{}, fmt.Errorf("post quote: %s", describe(resp.StatusCode, body))
	}
	var res imds.QuoteResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return imds.QuoteResult{}, fmt.Errorf("post quote: decode answer (HTTP %d): %w", resp.StatusCode, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK && res.Verified:
		a.log.InfoContext(ctx, "attestation verified", "stage", res.Stage, "user_data_released", res.UserDataReleased, "message", res.Message)
		return res, nil
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusForbidden:
		return res, fmt.Errorf("%w: %s", ErrRejected, res.Message)
	default:
		return res, fmt.Errorf("post quote: HTTP %d: %s", resp.StatusCode, res.Message)
	}
}

// do sends the request newReq builds, retrying with backoff while the
// transport fails (the IMDS may come up a moment after the agent) until ctx
// ends. Answers with any status are returned to the caller.
func (a *attester) do(ctx context.Context, newReq func() (*http.Request, error)) (*http.Response, error) {
	delay := a.backoff
	for {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		if a.userAgent != "" {
			req.Header.Set("User-Agent", a.userAgent)
		}
		resp, err := a.client.Do(req)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("imds not reachable within the timeout: %w", err)
		}
		a.log.WarnContext(ctx, "imds not reachable, retrying", "err", err, "in", delay)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("imds not reachable within the timeout: %w", err)
		case <-time.After(delay):
		}
		delay = min(delay*2, maxBackoff)
	}
}

// isJSON reports whether the answer carries a JSON body.
func isJSON(resp *http.Response) bool {
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// describe renders a non-JSON answer for an error message.
func describe(status int, body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > maxErrorBody {
		text = text[:maxErrorBody]
	}
	if status == http.StatusForbidden && text == "" {
		text = "the IMDS does not map this address to a VM"
	}
	if text == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, text)
}

// print writes the request as indented JSON.
func print(w io.Writer, req imds.QuoteRequest) error {
	data, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quote: %w", err)
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
