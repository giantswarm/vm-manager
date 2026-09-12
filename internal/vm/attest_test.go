package vm_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/attest"
	"github.com/giantswarm/vm-manager/internal/imds"
	"github.com/giantswarm/vm-manager/internal/vm/vmtest"
)

// detailedAttestor is what attest.Verifier looks like to the VM service: an
// Attestor that also keeps a Result per stage and forgets VMs.
type detailedAttestor struct {
	imds.NoopAttestor
	results map[string]attest.Result
	forgot  []string
}

func (a *detailedAttestor) Result(vmID string, stage imds.Stage) (attest.Result, bool) {
	r, ok := a.results[vmID+"/"+string(stage)]
	return r, ok
}

func (a *detailedAttestor) Forget(vmID string) { a.forgot = append(a.forgot, vmID) }

// restart closes the service and starts a new one on the same state dir,
// as a new process would: the network manager comes back fresh.
func (h *harness) restart() {
	h.t.Helper()
	require.NoError(h.t, h.svc.Close(h.ctx))
	h.nets = vmtest.NewNetworks(h.ev)
	h.start()
}

// restartWithPolicy restarts the service on a catalog whose image carries
// the given policy.json.
func (h *harness) restartWithPolicy(policy string) {
	h.t.Helper()
	img, err := h.imgs.Default()
	require.NoError(h.t, err)
	img.Policy = json.RawMessage(policy)
	h.imgs = vmtest.NewImages(h.imageDir, img)
	h.restart()
}

func TestPolicyFor(t *testing.T) {
	h := newHarness(t)
	v := h.create("plain")
	hexA, hexB := strings.Repeat("a", 64), strings.Repeat("b", 64)

	_, err := h.svc.PolicyFor(h.ctx, v.ID)
	assert.ErrorIs(t, err, attest.ErrNoPolicy, "the harness image has no policy")
	assert.ErrorContains(t, err, "has no policy.json")
	_, err = h.svc.PolicyFor(h.ctx, "nope")
	assert.ErrorIs(t, err, apierr.ErrNotFound)

	h.restartWithPolicy(`{"image_id":"giantswarm-vm-base","image_version":"0.1.0",
		"pcr11":{"enter-initrd":"` + hexA + `","enter-initrd:leave-initrd:sysinit:ready":"` + hexB + `"},
		"pcr13":{"1.32.0":"` + hexB + `"},
		"golden":{"sha256":{"4":"` + hexA + `"}}}`)
	p, err := h.svc.PolicyFor(h.ctx, v.ID)
	require.NoError(t, err)
	assert.Equal(t, hexA, p.PCR11[attest.PhaseInitrd])
	assert.Equal(t, hexB, p.PCR11[attest.PhaseReady])
	assert.Equal(t, map[int]string{4: hexA}, p.Golden[attest.Bank])
	assert.Equal(t, map[string]string{"1.32.0": hexB}, p.PCR13)
	assert.Equal(t, "1.32.0", v.KubernetesVersion, "the harness image's highest version")
	assert.Equal(t, v.KubernetesVersion, p.KubernetesVersion, "the VM's version selects the pcr13 entry")

	h.restartWithPolicy(`{"pcr11":{"enter-initrd":"` + hexA + `"}}`)
	_, err = h.svc.PolicyFor(h.ctx, v.ID)
	assert.ErrorIs(t, err, attest.ErrNoPolicy, "an invalid policy is no policy")
	assert.ErrorContains(t, err, "lacks phase")
}

func TestQuoteDetailsRecordedAndForgotten(t *testing.T) {
	h := newHarness(t)
	att := &detailedAttestor{results: map[string]attest.Result{}}
	h.attestor = att
	h.restart()

	s := h.spec("detailed")
	s.RequireAttestation = true
	v, err := h.svc.Create(h.ctx, s)
	require.NoError(t, err)
	h.install(v.ID)

	a := h.svc.IMDSDeps().Attestor
	nonce, err := a.Nonce(h.ctx, v.ID)
	require.NoError(t, err)
	att.results[v.ID+"/initrd"] = attest.Result{Verified: true, AKFingerprint: "ak-fp", PCRs: map[int]string{11: "ff", 0: "00"}, Learned: []int{0}}
	res, err := a.SubmitQuote(h.ctx, v.ID, imds.QuoteRequest{Stage: imds.StageInitrd, Nonce: nonce})
	require.NoError(t, err)
	require.True(t, res.Verified)

	check := func() {
		t.Helper()
		got, err := h.svc.Attestation(v.ID)
		require.NoError(t, err)
		require.NotNil(t, got.Initrd)
		assert.True(t, got.Initrd.Verified)
		assert.Equal(t, "ak-fp", got.Initrd.AKFingerprint)
		assert.Equal(t, map[int]string{11: "ff", 0: "00"}, got.Initrd.PCRs)
		assert.Equal(t, []int{0}, got.Initrd.Learned)
		assert.True(t, got.UserDataReleased)
		assert.Nil(t, got.Ready)
	}
	check()

	// The details are persisted with the record and survive a restart.
	h.restart()
	check()

	// A stage the attestor has no details for keeps the plain verdict.
	nonce, err = a.Nonce(h.ctx, v.ID)
	require.NoError(t, err)
	_, err = h.svc.IMDSDeps().Attestor.SubmitQuote(h.ctx, v.ID, imds.QuoteRequest{Stage: imds.StageReady, Nonce: nonce})
	require.NoError(t, err)
	got, _ := h.svc.Attestation(v.ID)
	require.NotNil(t, got.Ready)
	assert.Empty(t, got.Ready.AKFingerprint)
	assert.Nil(t, got.Ready.PCRs)

	data, err := json.Marshal(got.Initrd)
	require.NoError(t, err)
	assert.JSONEq(t, `{"verified":true,"message":"nonce matched; quote not verified (noop attestor)","at":"`+got.Initrd.At.Format("2006-01-02T15:04:05Z07:00")+`","akFingerprint":"ak-fp","pcrs":{"0":"00","11":"ff"},"learned":[0]}`, string(data))

	require.NoError(t, h.svc.Delete(h.ctx, v.ID))
	assert.Equal(t, []string{v.ID}, att.forgot)
}
