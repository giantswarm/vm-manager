package tpmquote_test

import (
	"crypto"
	"encoding/hex"
	"testing"

	"github.com/google/go-tpm/tpm2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/tpmquote"
	"github.com/giantswarm/vm-manager/internal/tpmquote/quotetest"
)

var initrdPCRs = []int{0, 1, 2, 3, 4, 5, 6, 7, 11}

// fixture is one simulator with an agent-style AK, a few extended PCRs and a
// quote over them.
type fixture struct {
	sim    *quotetest.Sim
	ak     *quotetest.AK
	nonce  string
	values map[int][]byte
	quote  []byte
	sig    []byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{sim: quotetest.Open(t), nonce: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"}
	f.ak = f.sim.NewAK(tpmquote.AKTemplate)
	f.sim.Extend(4, "boot loader")
	f.sim.Extend(7, "secure boot policy")
	f.sim.Extend(11, "uki")
	f.sim.Extend(11, "enter-initrd")
	f.values = f.sim.ReadPCRs(initrdPCRs...)
	nonce, err := hex.DecodeString(f.nonce)
	require.NoError(t, err)
	f.quote, f.sig = f.sim.Quote(f.ak, nonce, initrdPCRs...)
	return f
}

// verify runs the whole chain the verifier runs.
func verify(q *tpmquote.Quote, nonce string, values map[int][]byte) error {
	if err := tpmquote.VerifySignature(q); err != nil {
		return err
	}
	if err := tpmquote.VerifyNonce(q.ExtraData, nonce); err != nil {
		return err
	}
	return tpmquote.VerifyPCRs(q, values)
}

func TestParseAndVerify(t *testing.T) {
	f := newFixture(t)
	q, err := tpmquote.Parse(f.quote, f.sig, f.ak.PubBytes)
	require.NoError(t, err)
	assert.Equal(t, "sha256", q.Bank)
	assert.Equal(t, initrdPCRs, q.PCRs)
	assert.Equal(t, crypto.SHA256, q.Hash)
	assert.Equal(t, tpmquote.Fingerprint(&f.ak.Public), q.AKFingerprint)
	assert.Len(t, q.AKFingerprint, 64)
	require.NoError(t, verify(q, f.nonce, f.values))

	// Values for PCRs outside the selection do not disturb the digest.
	extra := f.sim.ReadPCRs(13)
	for k, v := range f.values {
		extra[k] = v
	}
	assert.NoError(t, tpmquote.VerifyPCRs(q, extra))

	digest, err := tpmquote.PCRDigest(crypto.SHA256, initrdPCRs, f.values)
	require.NoError(t, err)
	assert.Equal(t, q.PCRDigest, digest)

	// A TPM2B_PUBLIC wrapper is accepted and fingerprints identically.
	wrapped := tpm2.Marshal(tpm2.New2B(f.ak.Public))
	q2, err := tpmquote.Parse(f.quote, f.sig, wrapped)
	require.NoError(t, err)
	assert.Equal(t, q.AKFingerprint, q2.AKFingerprint)
	assert.NoError(t, verify(q2, f.nonce, f.values))
}

func TestTamper(t *testing.T) {
	f := newFixture(t)

	t.Run("nonce", func(t *testing.T) {
		q, err := tpmquote.Parse(f.quote, f.sig, f.ak.PubBytes)
		require.NoError(t, err)
		other := "ff" + f.nonce[2:]
		assert.ErrorIs(t, verify(q, other, f.values), tpmquote.ErrNonce)
		assert.ErrorIs(t, tpmquote.VerifyNonce(q.ExtraData, "not-hex"), tpmquote.ErrNonce)
		assert.ErrorIs(t, tpmquote.VerifyNonce(nil, ""), tpmquote.ErrNonce)
	})

	t.Run("pcr value", func(t *testing.T) {
		q, err := tpmquote.Parse(f.quote, f.sig, f.ak.PubBytes)
		require.NoError(t, err)
		values := map[int][]byte{}
		for k, v := range f.values {
			values[k] = v
		}
		values[4] = append([]byte(nil), values[4]...)
		values[4][0] ^= 1
		assert.ErrorIs(t, verify(q, f.nonce, values), tpmquote.ErrPCRDigest)

		delete(values, 11)
		assert.ErrorIs(t, tpmquote.VerifyPCRs(q, values), tpmquote.ErrPCRValue)
		values[11] = []byte{1, 2, 3}
		assert.ErrorIs(t, tpmquote.VerifyPCRs(q, values), tpmquote.ErrPCRValue)
	})

	t.Run("signature bit", func(t *testing.T) {
		sig := append([]byte(nil), f.sig...)
		sig[len(sig)-1] ^= 0x80
		q, err := tpmquote.Parse(f.quote, sig, f.ak.PubBytes)
		require.NoError(t, err)
		assert.ErrorIs(t, verify(q, f.nonce, f.values), tpmquote.ErrSignature)
	})

	t.Run("attest byte", func(t *testing.T) {
		quote := append([]byte(nil), f.quote...)
		quote[len(quote)-1] ^= 0x01 // last byte of the PCR digest
		q, err := tpmquote.Parse(quote, f.sig, f.ak.PubBytes)
		require.NoError(t, err)
		assert.ErrorIs(t, verify(q, f.nonce, f.values), tpmquote.ErrSignature)
	})

	t.Run("wrong key", func(t *testing.T) {
		other := f.sim.NewAK(tpmquote.AKTemplate)
		defer f.sim.Flush(other)
		q, err := tpmquote.Parse(f.quote, f.sig, other.PubBytes)
		require.NoError(t, err)
		assert.NotEqual(t, tpmquote.Fingerprint(&f.ak.Public), q.AKFingerprint)
		assert.ErrorIs(t, verify(q, f.nonce, f.values), tpmquote.ErrSignature)
	})

	t.Run("unrestricted key", func(t *testing.T) {
		ak := f.sim.NewAK(quotetest.Unrestricted())
		defer f.sim.Flush(ak)
		nonce, _ := hex.DecodeString(f.nonce)
		quote, sig := f.sim.Quote(ak, nonce, initrdPCRs...)
		_, err := tpmquote.Parse(quote, sig, ak.PubBytes)
		assert.ErrorIs(t, err, tpmquote.ErrAKAttributes)
		assert.ErrorContains(t, err, "restricted")
	})

	t.Run("decrypt key", func(t *testing.T) {
		// The SRK template is restricted+decrypt: a storage key, no signer.
		_, err := tpmquote.Parse(f.quote, f.sig, tpm2.Marshal(&tpm2.ECCSRKTemplate))
		assert.ErrorIs(t, err, tpmquote.ErrAKAttributes)
	})

	t.Run("magic", func(t *testing.T) {
		quote := append([]byte(nil), f.quote...)
		quote[0] ^= 0xff
		_, err := tpmquote.Parse(quote, f.sig, f.ak.PubBytes)
		assert.ErrorIs(t, err, tpmquote.ErrMagic)
	})

	t.Run("type", func(t *testing.T) {
		quote := append([]byte(nil), f.quote...)
		quote[5] = 0x17 // TPM_ST_ATTEST_CERTIFY
		_, err := tpmquote.Parse(quote, f.sig, f.ak.PubBytes)
		assert.ErrorIs(t, err, tpmquote.ErrType)
	})

	t.Run("malformed", func(t *testing.T) {
		_, err := tpmquote.Parse(f.quote[:20], f.sig, f.ak.PubBytes)
		assert.ErrorIs(t, err, tpmquote.ErrMalformed)
		_, err = tpmquote.Parse(f.quote, f.sig[:3], f.ak.PubBytes)
		assert.ErrorIs(t, err, tpmquote.ErrMalformed)
		_, err = tpmquote.Parse(f.quote, f.sig, []byte("nope"))
		assert.ErrorIs(t, err, tpmquote.ErrMalformed)
		_, err = tpmquote.Parse(nil, f.sig, f.ak.PubBytes)
		assert.ErrorIs(t, err, tpmquote.ErrMalformed)
	})
}

func TestBankHash(t *testing.T) {
	h, err := tpmquote.BankHash("sha256")
	require.NoError(t, err)
	assert.Equal(t, crypto.SHA256, h)
	_, err = tpmquote.BankHash("md5")
	assert.ErrorIs(t, err, tpmquote.ErrBank)
}
