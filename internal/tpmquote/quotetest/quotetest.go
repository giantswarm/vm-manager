// Package quotetest produces real TPM quotes on the go-tpm simulator for the
// tests of internal/tpmquote and internal/attest: it creates attestation
// keys, extends PCRs and quotes them the way the guest agent does.
package quotetest

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"github.com/google/go-tpm/tpm2/transport/simulator"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/tpmquote"
)

// Sim is one simulated TPM; Open starts it fresh (all PCRs zero) and closes
// it when the test ends. The simulator is a process-wide singleton: open one
// at a time, a second Open blocks until the first is closed. It has three
// transient object slots and creating a key needs a free one, so Flush AKs
// a test is done with before loading a third.
type Sim struct {
	t   *testing.T
	tpm transport.TPMCloser
	// srk is the owner-hierarchy storage primary the AKs live under, made
	// persistent on first use so it takes no transient slot.
	srk *tpm2.NamedHandle
}

// srkHandle is where the storage primary is persisted.
const srkHandle = tpm2.TPMHandle(0x81000001)

// AK is a signing key loaded in the simulator.
type AK struct {
	handle tpm2.TPMHandle
	name   tpm2.TPM2BName
	// Public is the key's TPMT_PUBLIC; PubBytes its marshalled form, what
	// the agent sends as ak_pub.
	Public   tpm2.TPMTPublic
	PubBytes []byte
}

// Open starts a simulator. The simulator is cgo; where the test binary was
// built with CGO_ENABLED=0 the test skips and names the reason, as the
// agent's tests do.
func Open(t *testing.T) *Sim {
	t.Helper()
	tpm, err := simulator.OpenSimulator()
	if err != nil && strings.Contains(err.Error(), "CGO") {
		t.Skipf("tpm simulator unavailable: %v", err)
	}
	require.NoError(t, err, "open tpm simulator")
	t.Cleanup(func() { _ = tpm.Close() })
	return &Sim{t: t, tpm: tpm}
}

// Transport exposes the simulator to code that drives a TPM itself, such
// as the guest agent's quote builder.
func (s *Sim) Transport() transport.TPM { return s.tpm }

// NewAK creates a key from template the way the guest agent does: a child
// of a tpm2.ECCSRKTemplate primary in the owner hierarchy, empty auth.
// Every call yields a distinct key; tpmquote.AKTemplate is what the agent
// uses.
func (s *Sim) NewAK(template tpm2.TPMTPublic) *AK {
	s.t.Helper()
	if s.srk == nil {
		rsp, err := tpm2.CreatePrimary{PrimaryHandle: tpm2.TPMRHOwner, InPublic: tpm2.New2B(tpm2.ECCSRKTemplate)}.Execute(s.tpm)
		require.NoError(s.t, err, "create srk")
		transient := tpm2.NamedHandle{Handle: rsp.ObjectHandle, Name: rsp.Name}
		_, err = tpm2.EvictControl{Auth: tpm2.TPMRHOwner, ObjectHandle: transient, PersistentHandle: srkHandle}.Execute(s.tpm)
		require.NoError(s.t, err, "persist srk")
		_, err = tpm2.FlushContext{FlushHandle: transient.Handle}.Execute(s.tpm)
		require.NoError(s.t, err, "flush transient srk")
		s.srk = &tpm2.NamedHandle{Handle: srkHandle, Name: rsp.Name}
	}
	created, err := tpm2.Create{ParentHandle: *s.srk, InPublic: tpm2.New2B(template)}.Execute(s.tpm)
	require.NoError(s.t, err, "create ak")
	loaded, err := tpm2.Load{ParentHandle: *s.srk, InPrivate: created.OutPrivate, InPublic: created.OutPublic}.Execute(s.tpm)
	require.NoError(s.t, err, "load ak")
	pub, err := created.OutPublic.Contents()
	require.NoError(s.t, err)
	return &AK{handle: loaded.ObjectHandle, name: loaded.Name, Public: *pub, PubBytes: tpm2.Marshal(pub)}
}

// Flush unloads an AK, freeing its transient slot.
func (s *Sim) Flush(ak *AK) {
	s.t.Helper()
	_, err := tpm2.FlushContext{FlushHandle: ak.handle}.Execute(s.tpm)
	require.NoError(s.t, err, "flush ak")
}

// Extend extends PCR index in the sha256 bank with sha256(data).
func (s *Sim) Extend(index int, data string) {
	s.t.Helper()
	digest := sha256.Sum256([]byte(data))
	_, err := tpm2.PCRExtend{
		PCRHandle: tpm2.AuthHandle{Handle: tpm2.TPMHandle(index), Auth: tpm2.PasswordAuth(nil)}, // #nosec G115 -- PCR indexes are small.
		Digests:   tpm2.TPMLDigestValues{Digests: []tpm2.TPMTHA{{HashAlg: tpm2.TPMAlgSHA256, Digest: digest[:]}}},
	}.Execute(s.tpm)
	require.NoError(s.t, err, "extend pcr %d", index)
}

// ReadPCRs returns the sha256 values of the given indexes.
func (s *Sim) ReadPCRs(indexes ...int) map[int][]byte {
	s.t.Helper()
	out := make(map[int][]byte, len(indexes))
	// TPM2_PCR_Read returns at most eight values per call.
	for start := 0; start < len(indexes); start += 8 {
		chunk := indexes[start:min(start+8, len(indexes))]
		rsp, err := tpm2.PCRRead{PCRSelectionIn: selection(chunk)}.Execute(s.tpm)
		require.NoError(s.t, err, "read pcrs")
		require.Len(s.t, rsp.PCRValues.Digests, len(chunk))
		for i, d := range rsp.PCRValues.Digests {
			out[chunk[i]] = d.Buffer
		}
	}
	return out
}

// Quote signs the sha256 bank at indexes with ak and nonce as qualifying
// data; it returns the TPMS_ATTEST bytes and the marshalled TPMT_SIGNATURE.
func (s *Sim) Quote(ak *AK, nonce []byte, indexes ...int) (quote, sig []byte) {
	s.t.Helper()
	rsp, err := tpm2.Quote{
		SignHandle:     tpm2.AuthHandle{Handle: ak.handle, Name: ak.name, Auth: tpm2.PasswordAuth(nil)},
		QualifyingData: tpm2.TPM2BData{Buffer: nonce},
		InScheme:       tpm2.TPMTSigScheme{Scheme: tpm2.TPMAlgNull},
		PCRSelect:      selection(indexes),
	}.Execute(s.tpm)
	require.NoError(s.t, err, "quote")
	return rsp.Quoted.Bytes(), tpm2.Marshal(&rsp.Signature)
}

// Unrestricted is AKTemplate without the restricted attribute: a key
// TPM2_Quote accepts but the verifier must not.
func Unrestricted() tpm2.TPMTPublic {
	t := tpmquote.AKTemplate
	t.ObjectAttributes.Restricted = false
	return t
}

// Extended is what Extend leaves in a PCR holding old: sha256(old ||
// sha256(data)), computed in software to predict policy values.
func Extended(old []byte, data string) []byte {
	digest := sha256.Sum256([]byte(data))
	sum := sha256.Sum256(append(append([]byte(nil), old...), digest[:]...))
	return sum[:]
}

func selection(indexes []int) tpm2.TPMLPCRSelection {
	return tpm2.TPMLPCRSelection{PCRSelections: []tpm2.TPMSPCRSelection{{
		Hash:      tpm2.TPMAlgSHA256,
		PCRSelect: tpm2.PCClientCompatible.PCRs(toUint(indexes)...),
	}}}
}

func toUint(indexes []int) []uint {
	out := make([]uint, len(indexes))
	for i, v := range indexes {
		out[i] = uint(v) // #nosec G115 -- PCR indexes are small non-negative test constants.
	}
	return out
}
