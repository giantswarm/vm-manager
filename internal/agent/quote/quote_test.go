package quote

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-tpm-tools/simulator"
	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/imds"
)

// newSimulator starts the Microsoft reference TPM. It needs cgo; without it
// (the architect CI image builds with CGO_ENABLED=0) the tests skip and
// name the reason. The simulator is a process-global singleton, so these
// tests must not run in parallel.
func newSimulator(t *testing.T) *simulator.Simulator {
	t.Helper()
	sim, err := simulator.Get()
	if err != nil {
		if strings.Contains(err.Error(), "CGO") {
			t.Skipf("tpm simulator unavailable: %v", err)
		}
		t.Fatalf("tpm simulator: %v", err)
	}
	t.Cleanup(func() { _ = sim.Close() })
	return sim
}

const nonce = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func TestEnsureAKIsIdempotentAcrossReboots(t *testing.T) {
	sim := newSimulator(t)
	tpm := transport.FromReadWriter(sim)

	first, err := EnsureAK(tpm)
	require.NoError(t, err)
	assert.Equal(t, AKHandle, first.Handle)
	assert.Equal(t, tpm2.TPMAlgECC, first.Public.Type)
	assert.True(t, first.Public.ObjectAttributes.Restricted)
	assert.True(t, first.Public.ObjectAttributes.SignEncrypt)
	assert.True(t, first.Public.ObjectAttributes.FixedTPM)

	// A reboot drops transient objects but keeps persistent ones.
	require.NoError(t, sim.Reset())

	second, err := EnsureAK(tpm)
	require.NoError(t, err)
	assert.Equal(t, first.Handle, second.Handle)
	assert.Equal(t, first.PublicBytes, second.PublicBytes)
	assert.Equal(t, first.Name, second.Name)

	// Nothing transient is left behind: the SRK and the loaded AK were flushed.
	caps, err := tpm2.GetCapability{
		Capability: tpm2.TPMCapHandles, Property: uint32(tpm2.TPMHTTransient) << 24, PropertyCount: 8,
	}.Execute(tpm)
	require.NoError(t, err)
	handles, err := caps.CapabilityData.Data.Handles()
	require.NoError(t, err)
	assert.Empty(t, handles.Handle)
}

func TestBuildQuoteVerifiesLocally(t *testing.T) {
	sim := newSimulator(t)
	tpm := transport.FromReadWriter(sim)

	dir := t.TempDir()
	fwLog := filepath.Join(dir, "binary_bios_measurements")
	require.NoError(t, os.WriteFile(fwLog, []byte("firmware-log"), 0o600))
	opts := Options{TPM: tpm, FirmwareLog: fwLog, UserspaceLog: filepath.Join(dir, "missing")}

	var seen = map[imds.Stage]imds.QuoteRequest{}
	for _, stage := range []imds.Stage{imds.StageInitrd, imds.StageReady} {
		t.Run(string(stage), func(t *testing.T) {
			req, err := Build(opts, stage, nonce)
			require.NoError(t, err)
			seen[stage] = req

			assert.Equal(t, stage, req.Stage)
			assert.Equal(t, nonce, req.Nonce)
			assert.Equal(t, []byte("firmware-log"), req.EventLog, "firmware log attached")
			assert.Nil(t, req.UserspaceLog, "missing userspace log is skipped")
			assert.NotEmpty(t, req.EKPub)

			want, _ := Selection(stage)
			values := req.PCRs[Bank]
			require.Len(t, req.PCRs, 1)
			require.Len(t, values, len(want))
			for _, idx := range want {
				assert.Len(t, values[strconv.Itoa(int(idx))], 2*sha256.Size, "pcr %d", idx)
			}
			verifyQuote(t, req, want)
		})
	}

	// The two selections differ only in PCR 13, and the AK does not change
	// between stages.
	_, initrdHas13 := seen[imds.StageInitrd].PCRs[Bank]["13"]
	_, readyHas13 := seen[imds.StageReady].PCRs[Bank]["13"]
	assert.False(t, initrdHas13)
	assert.True(t, readyHas13)
	assert.Equal(t, seen[imds.StageInitrd].AKPub, seen[imds.StageReady].AKPub)
	assert.Equal(t, seen[imds.StageInitrd].EKPub, seen[imds.StageReady].EKPub)
}

// verifyQuote does what the host verifier will do with the wire fields:
// decode TPMS_ATTEST, check magic, type and nonce, recompute the PCR digest
// from the reported values and verify the ECDSA signature with ak_pub.
func verifyQuote(t *testing.T, req imds.QuoteRequest, want []uint) {
	t.Helper()

	attest, err := tpm2.Unmarshal[tpm2.TPMSAttest](req.Quote)
	require.NoError(t, err)
	assert.Equal(t, tpm2.TPMGeneratedValue, attest.Magic)
	assert.Equal(t, tpm2.TPMSTAttestQuote, attest.Type)
	wantNonce, _ := hex.DecodeString(req.Nonce)
	assert.Equal(t, wantNonce, attest.ExtraData.Buffer, "nonce is the qualifying data")

	info, err := attest.Attested.Quote()
	require.NoError(t, err)
	assert.Equal(t, want, selected(info.PCRSelect, tpm2.TPMAlgSHA256))
	h := sha256.New()
	for _, idx := range want {
		v, err := hex.DecodeString(req.PCRs[Bank][strconv.Itoa(int(idx))])
		require.NoError(t, err)
		h.Write(v)
	}
	assert.Equal(t, h.Sum(nil), info.PCRDigest.Buffer, "digest over the reported PCRs in selection order")

	pub, err := tpm2.Unmarshal[tpm2.TPMTPublic](req.AKPub)
	require.NoError(t, err)
	point, err := pub.Unique.ECC()
	require.NoError(t, err)
	uncompressed := append(append([]byte{0x04}, point.X.Buffer...), point.Y.Buffer...)
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), uncompressed)
	require.NoError(t, err)
	sig, err := tpm2.Unmarshal[tpm2.TPMTSignature](req.Signature)
	require.NoError(t, err)
	assert.Equal(t, tpm2.TPMAlgECDSA, sig.SigAlg)
	ecc, err := sig.Signature.ECDSA()
	require.NoError(t, err)
	assert.Equal(t, tpm2.TPMAlgSHA256, ecc.Hash)
	digest := sha256.Sum256(req.Quote)
	assert.True(t, ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(ecc.SignatureR.Buffer), new(big.Int).SetBytes(ecc.SignatureS.Buffer)),
		"signature over TPMS_ATTEST verifies with ak_pub")

	ek, err := tpm2.Unmarshal[tpm2.TPMTPublic](req.EKPub)
	require.NoError(t, err)
	assert.Equal(t, tpm2.TPMAlgRSA, ek.Type)
}

func TestBuildRejectsBadInput(t *testing.T) {
	// Neither case touches the TPM, so no simulator is needed.
	_, err := Build(Options{}, "boot", nonce)
	assert.ErrorContains(t, err, `unknown stage "boot"`)
	_, err = Build(Options{}, imds.StageInitrd, "xyz")
	assert.ErrorContains(t, err, "nonce is not hex")
	_, err = Build(Options{}, imds.StageInitrd, "")
	assert.ErrorContains(t, err, "empty nonce")
}

func TestReadPCRsAllBanksAndRounds(t *testing.T) {
	sim := newSimulator(t)
	tpm := transport.FromReadWriter(sim)

	// Reading all 24 takes several TPM2_PCR_Read rounds (8 per answer in
	// the reference implementation).
	values, err := ReadPCRs(tpm, tpm2.TPMAlgSHA256, AllPCRs())
	require.NoError(t, err)
	require.Len(t, values, PCRCount)
	for idx, v := range values {
		assert.Len(t, v, sha256.Size, "pcr %d", idx)
	}
	assert.Equal(t, make([]byte, sha256.Size), values[0], "unextended pcr is zero")

	// An extend shows up in the next read.
	_, err = tpm2.PCRExtend{
		PCRHandle: tpm2.TPMHandle(16),
		Digests: tpm2.TPMLDigestValues{Digests: []tpm2.TPMTHA{{
			HashAlg: tpm2.TPMAlgSHA256, Digest: make([]byte, sha256.Size),
		}}},
	}.Execute(tpm)
	require.NoError(t, err)
	after, err := ReadPCRs(tpm, tpm2.TPMAlgSHA256, []uint{16})
	require.NoError(t, err)
	assert.NotEqual(t, values[16], after[16])

	sha1Values, err := ReadPCRs(tpm, tpm2.TPMAlgSHA1, []uint{0, 7})
	require.NoError(t, err)
	assert.Len(t, sha1Values[7], 20)
}

func TestEKPublicPrefersPersistedKey(t *testing.T) {
	sim := newSimulator(t)
	tpm := transport.FromReadWriter(sim)

	derived, err := EKPublic(tpm)
	require.NoError(t, err)

	// Persist the EK where a provisioned TPM would have it; the same
	// primary is derived from the endorsement seed, so the bytes match.
	ek, err := tpm2.CreatePrimary{PrimaryHandle: tpm2.TPMRHEndorsement, InPublic: tpm2.New2B(tpm2.RSAEKTemplate)}.Execute(tpm)
	require.NoError(t, err)
	_, err = tpm2.EvictControl{
		Auth:             tpm2.TPMRHOwner,
		ObjectHandle:     tpm2.NamedHandle{Handle: ek.ObjectHandle, Name: ek.Name},
		PersistentHandle: EKHandle,
	}.Execute(tpm)
	require.NoError(t, err)
	flush(tpm, ek.ObjectHandle)

	persisted, err := EKPublic(tpm)
	require.NoError(t, err)
	assert.Equal(t, derived, persisted)
}

func TestSelectionAndBank(t *testing.T) {
	initrd, err := Selection(imds.StageInitrd)
	require.NoError(t, err)
	assert.Equal(t, []uint{0, 1, 2, 3, 4, 5, 6, 7, 11}, initrd)
	ready, err := Selection(imds.StageReady)
	require.NoError(t, err)
	assert.Equal(t, []uint{0, 1, 2, 3, 4, 5, 6, 7, 11, 13}, ready)
	_, err = Selection("boot")
	assert.Error(t, err)

	alg, err := BankAlg("sha384")
	require.NoError(t, err)
	assert.Equal(t, tpm2.TPMAlgSHA384, alg)
	_, err = BankAlg("md5")
	assert.Error(t, err)

	assert.Equal(t, []uint{0, 7, 11}, selected(selection(tpm2.TPMAlgSHA256, []uint{11, 0, 7}), tpm2.TPMAlgSHA256))
	assert.Empty(t, selected(selection(tpm2.TPMAlgSHA256, []uint{0}), tpm2.TPMAlgSHA1))
}

func TestOpenMissingDevice(t *testing.T) {
	_, err := Open(Options{Device: filepath.Join(t.TempDir(), "tpmrm0")})
	assert.ErrorContains(t, err, "open tpm")
	_, err = Build(Options{Device: filepath.Join(t.TempDir(), "tpmrm0")}, imds.StageInitrd, nonce)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
