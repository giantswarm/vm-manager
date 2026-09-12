// Package attest is the imds.Attestor that verifies guest TPM quotes: it
// issues nonces (internal/nonce), checks each quote cryptographically with
// internal/tpmquote, pins the attestation key of a VM on its first verified
// initrd quote and compares the PCRs against the image's policy.
//
// # Verification order
//
// SubmitQuote rejects at the first failing step and says which one in
// QuoteResult.Message (expected and observed values included, nothing
// secret):
//
//  1. the nonce was issued to this VM, is unexpired and unused;
//  2. the VM's image has a policy (PolicyProvider);
//  3. quote, signature and ak_pub parse; the AK is a fixed, restricted
//     signing key (tpmquote.Parse);
//  4. the signature verifies with ak_pub, the qualifying data is the nonce,
//     the sha256 bank sent reproduces the quoted PCR digest and covers the
//     PCRs the stage needs (0-7, 11; plus 13 at ready);
//  5. the AK equals the one pinned for this VM (trust on first use: the
//     first verified initrd quote enrolls it; a ready quote without an
//     enrolled AK is rejected);
//  6. PCR 11 equals the policy's value for the stage's phase path;
//  7. PCRs 0-7 equal the golden values; a PCR without a golden value is
//     accepted only in learn mode (Options.LearnGolden) and reported in
//     Result.Learned; PCR 13 is compared at the ready stage against the
//     policy's pcr13 entry for the VM's Kubernetes version, else against
//     the golden value, and recorded when the policy has neither.
//
// Every verdict is kept per VM and stage (Results) for get_vm_attestation
// and `vm-manager image golden`.
package attest

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/vm-manager/internal/imds"
	"github.com/giantswarm/vm-manager/internal/nonce"
	"github.com/giantswarm/vm-manager/internal/tpmquote"
)

// PolicyProvider hands the verifier the policy of the image a VM runs.
// Wrap ErrNoPolicy when the image has none: the quote is then rejected
// with the reason; any other error fails the request.
type PolicyProvider interface {
	PolicyFor(ctx context.Context, vmID string) (Policy, error)
}

// PolicyProviderFunc adapts a function to PolicyProvider.
type PolicyProviderFunc func(ctx context.Context, vmID string) (Policy, error)

// PolicyFor implements PolicyProvider.
func (f PolicyProviderFunc) PolicyFor(ctx context.Context, vmID string) (Policy, error) {
	return f(ctx, vmID)
}

// Options configure a Verifier; Policies is required.
type Options struct {
	Policies PolicyProvider
	// LearnGolden accepts PCRs 0-7 without a golden value and records the
	// observed values (Result.Learned); for bring-up of a new image or
	// firmware, never for production.
	LearnGolden bool
	// Logger for verdicts and AK enrolment; nil uses slog.Default().
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Result is the verifier's verdict on one stage of one VM.
type Result struct {
	Verified bool      `json:"verified"`
	Message  string    `json:"message,omitempty"`
	At       time.Time `json:"at"`
	// AKFingerprint identifies the key that signed the quote
	// (tpmquote.Fingerprint), set once the quote parsed.
	AKFingerprint string `json:"akFingerprint,omitempty"`
	// PCRs are the quoted sha256 values by index, set once the PCR digest
	// verified; the image golden command reads them from the ready stage.
	PCRs map[int]string `json:"pcrs,omitempty"`
	// Learned lists the PCRs accepted without a golden value.
	Learned []int `json:"learned,omitempty"`
}

// pinMismatch is the rejection for a quote signed by another key than the
// one enrolled for the VM.
const pinMismatch = "ak %s is not the key enrolled for this vm (%s)"

// Verifier implements imds.Attestor.
type Verifier struct {
	opts   Options
	log    *slog.Logger
	nonces *nonce.Store

	mu  sync.Mutex
	vms map[string]*vmState
}

type vmState struct {
	// ak is the pinned AK fingerprint, empty until an initrd quote verified.
	ak      string
	results map[imds.Stage]Result
}

// New builds a Verifier.
func New(opts Options) (*Verifier, error) {
	if opts.Policies == nil {
		return nil, errors.New("attest: Options.Policies is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Verifier{opts: opts, log: opts.Logger, nonces: nonce.New(nonce.WithClock(opts.Now)), vms: make(map[string]*vmState)}, nil
}

// Nonce implements imds.Attestor.
func (v *Verifier) Nonce(_ context.Context, vmID string) (string, error) {
	return v.nonces.Issue(vmID)
}

// SubmitQuote implements imds.Attestor; see the package documentation for
// the order of checks. A rejected quote is a QuoteResult with Verified
// false; only a failing PolicyProvider returns an error.
func (v *Verifier) SubmitQuote(ctx context.Context, vmID string, req imds.QuoteRequest) (imds.QuoteResult, error) {
	res := Result{At: v.opts.Now()}
	reject := func(format string, args ...any) (imds.QuoteResult, error) {
		res.Message = fmt.Sprintf(format, args...)
		v.store(vmID, req.Stage, res)
		v.log.Warn("attestation rejected", "vm", vmID, "stage", req.Stage, "ak", short(res.AKFingerprint), "reason", res.Message)
		return imds.QuoteResult{Message: res.Message}, nil
	}

	if !v.nonces.Consume(vmID, req.Nonce) {
		return reject("unknown or expired nonce")
	}
	if !req.Stage.Valid() {
		return reject("unknown stage %q", req.Stage)
	}
	policy, err := v.opts.Policies.PolicyFor(ctx, vmID)
	if err != nil {
		if errors.Is(err, ErrNoPolicy) {
			return reject("%v", err)
		}
		return imds.QuoteResult{}, fmt.Errorf("attestation policy for vm %s: %w", vmID, err)
	}

	q, err := tpmquote.Parse(req.Quote, req.Signature, req.AKPub)
	if err != nil {
		return reject("quote: %v", err)
	}
	res.AKFingerprint = q.AKFingerprint
	if err := tpmquote.VerifySignature(q); err != nil {
		return reject("signature: %v", err)
	}
	if err := tpmquote.VerifyNonce(q.ExtraData, req.Nonce); err != nil {
		return reject("nonce: quote qualifying data does not match the nonce")
	}
	if q.Bank != Bank {
		return reject("quote selects pcr bank %s, want %s", q.Bank, Bank)
	}
	values, err := decodePCRs(req.PCRs[Bank], q.PCRs)
	if err != nil {
		return reject("%v", err)
	}
	if err := tpmquote.VerifyPCRs(q, values); err != nil {
		return reject("pcr digest: %v", err)
	}
	res.PCRs = hexPCRs(values)
	if missing := missingPCRs(q.PCRs, RequiredPCRs(req.Stage)); len(missing) > 0 {
		return reject("quote covers pcrs %s but stage %s also needs %s", indexList(q.PCRs), req.Stage, indexList(missing))
	}

	if pinned, ok := v.pinned(vmID); ok && pinned != q.AKFingerprint {
		return reject(pinMismatch, short(q.AKFingerprint), short(pinned))
	}

	phase := PhaseFor(req.Stage)
	if want, got := strings.ToLower(policy.PCR11[phase]), res.PCRs[PCRUKI]; want != got {
		return reject("pcr 11 mismatch for phase %s: expected %s, got %s", phase, want, got)
	}
	var mismatched, unknown, learned []string
	sysext := ""
	compare := func(index int) {
		got := res.PCRs[index]
		want, kubernetes, ok := policy.expected(index)
		switch {
		case !ok && (v.opts.LearnGolden || index == PCRSysext):
			res.Learned = append(res.Learned, index)
			learned = append(learned, fmt.Sprintf("%d=%s", index, got))
		case !ok:
			unknown = append(unknown, fmt.Sprint(index))
		case want != got && kubernetes != "":
			mismatched = append(mismatched, fmt.Sprintf("pcr %d expected %s for kubernetes %s, got %s", index, want, kubernetes, got))
		case want != got:
			mismatched = append(mismatched, fmt.Sprintf("pcr %d expected %s, got %s", index, want, got))
		case kubernetes != "":
			sysext = kubernetes
		}
	}
	for _, i := range firmwarePCRs {
		compare(i)
	}
	if req.Stage == imds.StageReady {
		compare(PCRSysext)
	}
	if len(mismatched) > 0 {
		return reject("golden mismatch: %s", strings.Join(mismatched, "; "))
	}
	if len(unknown) > 0 {
		return reject("no golden value for pcr %s in the image policy: record one with `vm-manager image golden` or run with --attestation-learn-golden", strings.Join(unknown, ","))
	}

	res.Verified = true
	res.Message = fmt.Sprintf("verified: ak %s, pcr 11 phase %s", short(q.AKFingerprint), phase)
	if sysext != "" {
		res.Message += ", pcr 13 kubernetes " + sysext
	}
	if len(learned) > 0 {
		res.Message += "; accepted without golden value: " + strings.Join(learned, " ")
	}
	if !v.enroll(vmID, req.Stage, q.AKFingerprint) {
		res.Verified = false
		if pinned, ok := v.pinned(vmID); ok {
			return reject(pinMismatch, short(q.AKFingerprint), short(pinned))
		}
		return reject("no ak enrolled for this vm: an initrd-stage quote must verify first")
	}
	v.store(vmID, req.Stage, res)
	if len(res.Learned) > 0 {
		v.log.Info("attestation accepted pcrs without golden value", "vm", vmID, "stage", req.Stage, "learn", v.opts.LearnGolden, "pcrs", strings.Join(learned, " "))
	}
	return imds.QuoteResult{Verified: true, Message: res.Message}, nil
}

// Results returns the last verdict per stage for a VM, nil when none.
func (v *Verifier) Results(vmID string) map[imds.Stage]Result {
	v.mu.Lock()
	defer v.mu.Unlock()
	st, ok := v.vms[vmID]
	if !ok {
		return nil
	}
	out := make(map[imds.Stage]Result, len(st.results))
	for stage, r := range st.results {
		out[stage] = r.clone()
	}
	return out
}

// Result returns the last verdict for one stage of a VM.
func (v *Verifier) Result(vmID string, stage imds.Stage) (Result, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	st, ok := v.vms[vmID]
	if !ok {
		return Result{}, false
	}
	r, ok := st.results[stage]
	return r.clone(), ok
}

// Forget drops the pinned AK, results and nonces of a VM; the VM service
// calls it when the VM is deleted.
func (v *Verifier) Forget(vmID string) {
	v.mu.Lock()
	delete(v.vms, vmID)
	v.mu.Unlock()
	v.nonces.Forget(vmID)
}

func (v *Verifier) pinned(vmID string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	st, ok := v.vms[vmID]
	return st.fingerprint(), ok && st.ak != ""
}

// enroll checks the pin under the lock a verified quote is recorded under:
// an initrd quote enrolls the first AK, every quote must match the pin.
func (v *Verifier) enroll(vmID string, stage imds.Stage, fingerprint string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	st := v.stateLocked(vmID)
	switch {
	case st.ak == "" && stage == imds.StageInitrd:
		st.ak = fingerprint
		v.log.Info("attestation key enrolled on first use", "vm", vmID, "ak", fingerprint)
		return true
	case st.ak == "":
		return false
	default:
		return st.ak == fingerprint
	}
}

func (v *Verifier) store(vmID string, stage imds.Stage, r Result) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stateLocked(vmID).results[stage] = r
}

func (v *Verifier) stateLocked(vmID string) *vmState {
	st, ok := v.vms[vmID]
	if !ok {
		st = &vmState{results: make(map[imds.Stage]Result)}
		v.vms[vmID] = st
	}
	return st
}

func (st *vmState) fingerprint() string {
	if st == nil {
		return ""
	}
	return st.ak
}

func (r Result) clone() Result {
	if r.PCRs != nil {
		pcrs := make(map[int]string, len(r.PCRs))
		for k, v := range r.PCRs {
			pcrs[k] = v
		}
		r.PCRs = pcrs
	}
	r.Learned = append([]int(nil), r.Learned...)
	return r
}

// decodePCRs turns the request's sha256 bank into bytes for the indexes
// the quote selects; every selected index must be present.
func decodePCRs(bank map[string]string, selected []int) (map[int][]byte, error) {
	values := make(map[int][]byte, len(selected))
	for _, i := range selected {
		hexValue, ok := bank[fmt.Sprint(i)]
		if !ok {
			return nil, fmt.Errorf("pcrs.%s lacks index %d which the quote covers", Bank, i)
		}
		b, err := hex.DecodeString(hexValue)
		if err != nil {
			return nil, fmt.Errorf("pcrs.%s[%d] is not hex", Bank, i)
		}
		values[i] = b
	}
	return values, nil
}

func hexPCRs(values map[int][]byte) map[int]string {
	out := make(map[int]string, len(values))
	for i, b := range values {
		out[i] = hex.EncodeToString(b)
	}
	return out
}

func missingPCRs(have, need []int) []int {
	set := make(map[int]bool, len(have))
	for _, i := range have {
		set[i] = true
	}
	var missing []int
	for _, i := range need {
		if !set[i] {
			missing = append(missing, i)
		}
	}
	sort.Ints(missing)
	return missing
}

// short abbreviates a fingerprint for messages and logs.
func short(fingerprint string) string {
	if len(fingerprint) > 16 {
		return fingerprint[:16]
	}
	return fingerprint
}
