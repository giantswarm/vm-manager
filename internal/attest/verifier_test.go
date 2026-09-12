package attest_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/agent/quote"
	"github.com/giantswarm/vm-manager/internal/attest"
	"github.com/giantswarm/vm-manager/internal/imds"
	"github.com/giantswarm/vm-manager/internal/tpmquote"
	"github.com/giantswarm/vm-manager/internal/tpmquote/quotetest"
)

const vmID = "vm-1"

// env is a simulated guest booted to the initrd stage next to a verifier
// whose policy was computed for exactly that guest: PCR 11 values predicted
// in software for both phase paths, golden values read from the TPM.
type env struct {
	t      *testing.T
	ctx    context.Context
	sim    *quotetest.Sim
	ak     *quotetest.AK
	policy attest.Policy
	// policyErr, when set, is what the provider returns instead.
	policyErr error
	now       time.Time
	v         *attest.Verifier
}

func newEnv(t *testing.T, learn bool) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), sim: quotetest.Open(t), now: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
	e.ak = e.sim.NewAK(tpmquote.AKTemplate)
	e.sim.Extend(0, "firmware")
	e.sim.Extend(4, "systemd-boot")
	e.sim.Extend(7, "secure boot db")
	e.sim.Extend(11, "uki")
	e.sim.Extend(11, "enter-initrd")
	values := e.sim.ReadPCRs(attest.RequiredPCRs(imds.StageInitrd)...)

	ready := values[11]
	for _, phase := range []string{"leave-initrd", "sysinit", "ready"} {
		ready = quotetest.Extended(ready, phase)
	}
	e.policy = attest.Policy{
		PCR11:  map[string]string{attest.PhaseInitrd: hex.EncodeToString(values[11]), attest.PhaseReady: hex.EncodeToString(ready)},
		Golden: map[string]map[int]string{attest.Bank: {}},
	}
	for i := 0; i <= 7; i++ {
		e.policy.Golden[attest.Bank][i] = hex.EncodeToString(values[i])
	}
	require.NoError(t, e.policy.Validate())

	var err error
	e.v, err = attest.New(attest.Options{
		Policies: attest.PolicyProviderFunc(func(_ context.Context, id string) (attest.Policy, error) {
			require.Equal(t, vmID, id)
			if e.policyErr != nil {
				return attest.Policy{}, e.policyErr
			}
			return e.policy, nil
		}),
		LearnGolden: learn,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:         func() time.Time { return e.now },
	})
	require.NoError(t, err)
	return e
}

// toReady moves the guest on: the remaining phases into PCR 11, a sysext
// into PCR 13.
func (e *env) toReady() {
	for _, phase := range []string{"leave-initrd", "sysinit", "ready"} {
		e.sim.Extend(11, phase)
	}
	e.sim.Extend(13, "kubernetes sysext")
}

// request builds what the agent posts: a fresh nonce, a quote over the
// stage's PCRs with ak and the PCR values read from the TPM.
func (e *env) request(ak *quotetest.AK, stage imds.Stage, pcrs ...int) imds.QuoteRequest {
	e.t.Helper()
	if len(pcrs) == 0 {
		pcrs = attest.RequiredPCRs(stage)
	}
	nonce, err := e.v.Nonce(e.ctx, vmID)
	require.NoError(e.t, err)
	raw, err := hex.DecodeString(nonce)
	require.NoError(e.t, err)
	quote, sig := e.sim.Quote(ak, raw, pcrs...)
	bank := map[string]string{}
	for i, v := range e.sim.ReadPCRs(pcrs...) {
		bank[fmt.Sprint(i)] = hex.EncodeToString(v)
	}
	return imds.QuoteRequest{Stage: stage, Nonce: nonce, AKPub: ak.PubBytes, Quote: quote, Signature: sig,
		PCRs: map[string]map[string]string{attest.Bank: bank}}
}

func (e *env) submit(req imds.QuoteRequest) imds.QuoteResult {
	e.t.Helper()
	res, err := e.v.SubmitQuote(e.ctx, vmID, req)
	require.NoError(e.t, err)
	return res
}

func TestVerifierAcceptsInitrdAndReady(t *testing.T) {
	e := newEnv(t, false)

	res := e.submit(e.request(e.ak, imds.StageInitrd))
	assert.True(t, res.Verified, res.Message)
	assert.Contains(t, res.Message, "verified: ak "+tpmquote.Fingerprint(&e.ak.Public)[:16])
	assert.Contains(t, res.Message, "phase enter-initrd")

	r, ok := e.v.Result(vmID, imds.StageInitrd)
	require.True(t, ok)
	assert.True(t, r.Verified)
	assert.Equal(t, tpmquote.Fingerprint(&e.ak.Public), r.AKFingerprint)
	assert.Len(t, r.PCRs, 9)
	assert.Equal(t, e.policy.PCR11[attest.PhaseInitrd], r.PCRs[11])
	assert.Empty(t, r.Learned)
	assert.Equal(t, e.now, r.At)

	// Ready: PCR 13 has no golden value yet and is recorded, not required.
	e.toReady()
	res = e.submit(e.request(e.ak, imds.StageReady))
	assert.True(t, res.Verified, res.Message)
	assert.Contains(t, res.Message, "accepted without golden value: 13=")
	r, ok = e.v.Result(vmID, imds.StageReady)
	require.True(t, ok)
	assert.Equal(t, []int{13}, r.Learned)
	assert.Len(t, r.PCRs, 10)
	assert.Equal(t, e.policy.PCR11[attest.PhaseReady], r.PCRs[11], "software-predicted phase path matches the TPM")

	// Once golden has PCR 13 it is compared.
	e.policy.Golden[attest.Bank][13] = r.PCRs[13]
	res = e.submit(e.request(e.ak, imds.StageReady))
	assert.True(t, res.Verified, res.Message)
	assert.NotContains(t, res.Message, "without golden")
	e.policy.Golden[attest.Bank][13] = e.policy.Golden[attest.Bank][0]
	res = e.submit(e.request(e.ak, imds.StageReady))
	assert.False(t, res.Verified)
	assert.Contains(t, res.Message, "golden mismatch: pcr 13 expected "+e.policy.Golden[attest.Bank][0])

	results := e.v.Results(vmID)
	assert.Len(t, results, 2)
	assert.True(t, results[imds.StageInitrd].Verified)
	assert.False(t, results[imds.StageReady].Verified)

	// The wire shape of a result, what get_vm_attestation carries.
	data, err := json.Marshal(results[imds.StageInitrd])
	require.NoError(t, err)
	var shape map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &shape))
	for _, key := range []string{"verified", "message", "at", "akFingerprint", "pcrs"} {
		assert.Contains(t, shape, key)
	}
	var pcrs map[string]string
	require.NoError(t, json.Unmarshal(shape["pcrs"], &pcrs))
	assert.Len(t, pcrs["11"], 64)
	readyGolden := attest.GoldenFromPCRs(r.PCRs)
	assert.Len(t, readyGolden, 9, "pcrs 0-7 and 13")
	delete(readyGolden, 13)
	assert.Equal(t, attest.GoldenFromPCRs(results[imds.StageInitrd].PCRs), readyGolden, "pcrs 0-7 do not move between the stages")
}

func TestVerifierRejectsPolicyMismatch(t *testing.T) {
	t.Run("pcr 11", func(t *testing.T) {
		e := newEnv(t, false)
		want := e.policy.PCR11[attest.PhaseReady]
		e.policy.PCR11[attest.PhaseInitrd] = want
		res := e.submit(e.request(e.ak, imds.StageInitrd))
		assert.False(t, res.Verified)
		assert.Contains(t, res.Message, "pcr 11 mismatch for phase enter-initrd: expected "+want+", got ")
	})

	t.Run("golden", func(t *testing.T) {
		e := newEnv(t, false)
		e.policy.Golden[attest.Bank][4] = e.policy.Golden[attest.Bank][1]
		res := e.submit(e.request(e.ak, imds.StageInitrd))
		assert.False(t, res.Verified)
		assert.Contains(t, res.Message, "golden mismatch: pcr 4 expected "+e.policy.Golden[attest.Bank][1]+", got ")
		assert.NotContains(t, res.Message, "pcr 5")
	})

	t.Run("golden missing", func(t *testing.T) {
		e := newEnv(t, false)
		delete(e.policy.Golden[attest.Bank], 0)
		delete(e.policy.Golden[attest.Bank], 7)
		res := e.submit(e.request(e.ak, imds.StageInitrd))
		assert.False(t, res.Verified)
		assert.Contains(t, res.Message, "no golden value for pcr 0,7 in the image policy")
		assert.Contains(t, res.Message, "--attestation-learn-golden")
		r, _ := e.v.Result(vmID, imds.StageInitrd)
		assert.Len(t, r.PCRs, 9, "observed values are kept even on rejection")
	})

	t.Run("no policy", func(t *testing.T) {
		e := newEnv(t, false)
		e.policyErr = fmt.Errorf("%w: image x has no policy.json", attest.ErrNoPolicy)
		res := e.submit(e.request(e.ak, imds.StageInitrd))
		assert.False(t, res.Verified)
		assert.Equal(t, "no attestation policy: image x has no policy.json", res.Message)

		e.policyErr = errors.New("catalog offline")
		_, err := e.v.SubmitQuote(e.ctx, vmID, e.request(e.ak, imds.StageInitrd))
		assert.ErrorContains(t, err, "catalog offline")
	})
}

func TestVerifierLearnGolden(t *testing.T) {
	e := newEnv(t, true)
	golden := e.policy.Golden[attest.Bank]
	e.policy.Golden = nil

	res := e.submit(e.request(e.ak, imds.StageInitrd))
	assert.True(t, res.Verified, res.Message)
	assert.Contains(t, res.Message, "accepted without golden value: 0="+golden[0])
	r, _ := e.v.Result(vmID, imds.StageInitrd)
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7}, r.Learned)
	for i, want := range golden {
		assert.Equal(t, want, r.PCRs[i], "pcr %d", i)
	}

	e.toReady()
	res = e.submit(e.request(e.ak, imds.StageReady))
	assert.True(t, res.Verified, res.Message)
	r, _ = e.v.Result(vmID, imds.StageReady)
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 13}, r.Learned)
	assert.Len(t, attest.GoldenFromPCRs(r.PCRs), 9, "everything image golden needs")

	// Learn mode does not excuse a wrong value.
	e.policy.Golden = map[string]map[int]string{attest.Bank: {3: golden[0]}}
	res = e.submit(e.request(e.ak, imds.StageReady))
	assert.False(t, res.Verified)
	assert.Contains(t, res.Message, "golden mismatch: pcr 3")
}

func TestVerifierPinsAK(t *testing.T) {
	e := newEnv(t, false)
	other := e.sim.NewAK(tpmquote.AKTemplate)
	fp, otherFP := tpmquote.Fingerprint(&e.ak.Public), tpmquote.Fingerprint(&other.Public)
	require.NotEqual(t, fp, otherFP)

	// The guest is up; this test is about keys, so let both stages accept
	// the ready phase path in PCR 11.
	e.toReady()
	e.policy.PCR11[attest.PhaseInitrd] = e.policy.PCR11[attest.PhaseReady]

	// Ready before any initrd quote: nothing is pinned yet, so nothing is trusted.
	res := e.submit(e.request(e.ak, imds.StageReady))
	assert.False(t, res.Verified)
	assert.Equal(t, "no ak enrolled for this vm: an initrd-stage quote must verify first", res.Message)

	// A failed initrd quote does not enroll its key.
	golden := e.policy.Golden[attest.Bank]
	golden[0], golden[1] = golden[1], golden[0]
	res = e.submit(e.request(other, imds.StageInitrd))
	assert.False(t, res.Verified)
	assert.Contains(t, res.Message, "golden mismatch")
	golden[0], golden[1] = golden[1], golden[0]
	res = e.submit(e.request(other, imds.StageReady))
	assert.Contains(t, res.Message, "no ak enrolled")

	// The first verified initrd quote enrolls; from then on only that key.
	res = e.submit(e.request(e.ak, imds.StageInitrd))
	assert.True(t, res.Verified, res.Message)
	res = e.submit(e.request(other, imds.StageInitrd))
	assert.False(t, res.Verified)
	assert.Equal(t, fmt.Sprintf("ak %s is not the key enrolled for this vm (%s)", otherFP[:16], fp[:16]), res.Message)
	res = e.submit(e.request(other, imds.StageReady))
	assert.False(t, res.Verified)
	assert.Contains(t, res.Message, "not the key enrolled")
	res = e.submit(e.request(e.ak, imds.StageReady))
	assert.True(t, res.Verified, res.Message)
	r, _ := e.v.Result(vmID, imds.StageInitrd)
	assert.False(t, r.Verified, "the last initrd verdict is the impostor's rejection")
	assert.Equal(t, otherFP, r.AKFingerprint)

	// Forget clears the pin; the VM is a stranger again.
	e.v.Forget(vmID)
	assert.Nil(t, e.v.Results(vmID))
	res = e.submit(e.request(e.ak, imds.StageReady))
	assert.Contains(t, res.Message, "no ak enrolled")
}

func TestVerifierRejectsBadQuotes(t *testing.T) {
	e := newEnv(t, false)

	t.Run("nonce replay and binding", func(t *testing.T) {
		req := e.request(e.ak, imds.StageInitrd)
		assert.True(t, e.submit(req).Verified)
		res := e.submit(req)
		assert.False(t, res.Verified)
		assert.Equal(t, "unknown or expired nonce", res.Message)

		req = e.request(e.ak, imds.StageInitrd)
		res, err := e.v.SubmitQuote(e.ctx, "vm-2", req)
		require.NoError(t, err)
		assert.Equal(t, "unknown or expired nonce", res.Message, "nonce is bound to the VM")

		req = e.request(e.ak, imds.StageInitrd)
		e.now = e.now.Add(imds.NonceTTL + time.Second)
		assert.Equal(t, "unknown or expired nonce", e.submit(req).Message)
	})

	t.Run("nonce not in quote", func(t *testing.T) {
		req := e.request(e.ak, imds.StageInitrd)
		other := e.request(e.ak, imds.StageInitrd)
		req.Quote, req.Signature = other.Quote, other.Signature
		res := e.submit(req)
		assert.False(t, res.Verified)
		assert.Equal(t, "nonce: quote qualifying data does not match the nonce", res.Message)
	})

	t.Run("pcr value tampered", func(t *testing.T) {
		req := e.request(e.ak, imds.StageInitrd)
		req.PCRs[attest.Bank]["4"] = req.PCRs[attest.Bank]["0"]
		res := e.submit(req)
		assert.Contains(t, res.Message, "pcr digest: pcr digest mismatch")
	})

	t.Run("pcr value missing", func(t *testing.T) {
		req := e.request(e.ak, imds.StageInitrd)
		delete(req.PCRs[attest.Bank], "11")
		assert.Equal(t, "pcrs.sha256 lacks index 11 which the quote covers", e.submit(req).Message)
		req = e.request(e.ak, imds.StageInitrd)
		req.PCRs = map[string]map[string]string{"sha1": {}}
		assert.Contains(t, e.submit(req).Message, "pcrs.sha256 lacks index 0")
	})

	t.Run("coverage", func(t *testing.T) {
		req := e.request(e.ak, imds.StageInitrd, 0, 1, 2, 3, 4, 5, 6, 7)
		assert.Equal(t, "quote covers pcrs 0,1,2,3,4,5,6,7 but stage initrd also needs 11", e.submit(req).Message)
	})

	t.Run("signature", func(t *testing.T) {
		req := e.request(e.ak, imds.StageInitrd)
		req.Signature[len(req.Signature)-1] ^= 1
		assert.Equal(t, "signature: signature does not verify", e.submit(req).Message)
	})

	t.Run("unrestricted ak", func(t *testing.T) {
		ak := e.sim.NewAK(quotetest.Unrestricted())
		defer e.sim.Flush(ak)
		res := e.submit(e.request(ak, imds.StageInitrd))
		assert.Contains(t, res.Message, "quote: attestation key attributes: [restricted] not set")
	})

	t.Run("garbage", func(t *testing.T) {
		req := e.request(e.ak, imds.StageInitrd)
		req.Quote = []byte("not a quote")
		assert.Contains(t, e.submit(req).Message, "quote: quote magic is not TPM_GENERATED_VALUE")
	})

	t.Run("stage", func(t *testing.T) {
		req := e.request(e.ak, imds.StageInitrd)
		req.Stage = "boot"
		assert.Equal(t, `unknown stage "boot"`, e.submit(req).Message)
	})
}

// TestVerifierAcceptsAgentQuotes drives the guest agent's own quote builder
// (internal/agent/quote, what vm-agent attest posts) against the verifier:
// the two halves of the protocol meet on one simulator.
func TestVerifierAcceptsAgentQuotes(t *testing.T) {
	e := newEnv(t, false)
	// No event logs: the host's are not the guest's and may be unreadable.
	none := filepath.Join(t.TempDir(), "absent")
	agent := quote.Options{TPM: e.sim.Transport(), FirmwareLog: none, UserspaceLog: none}
	build := func(stage imds.Stage) imds.QuoteRequest {
		t.Helper()
		nonce, err := e.v.Nonce(e.ctx, vmID)
		require.NoError(t, err)
		req, err := quote.Build(agent, stage, nonce)
		require.NoError(t, err)
		return req
	}

	res := e.submit(build(imds.StageInitrd))
	assert.True(t, res.Verified, res.Message)
	r, ok := e.v.Result(vmID, imds.StageInitrd)
	require.True(t, ok)
	assert.Len(t, r.AKFingerprint, 64)
	assert.Len(t, r.PCRs, 9)

	// The persistent AK is the same key on the next quote, so the pin holds.
	e.toReady()
	res = e.submit(build(imds.StageReady))
	assert.True(t, res.Verified, res.Message)
	ready, _ := e.v.Result(vmID, imds.StageReady)
	assert.Equal(t, r.AKFingerprint, ready.AKFingerprint)
	assert.Equal(t, []int{13}, ready.Learned)

	// The verifier's own test key is a different AK and is turned away.
	res = e.submit(e.request(e.ak, imds.StageReady))
	assert.False(t, res.Verified)
	assert.Contains(t, res.Message, "not the key enrolled")
}

func TestNewRequiresPolicies(t *testing.T) {
	_, err := attest.New(attest.Options{})
	assert.ErrorContains(t, err, "Policies")
}
