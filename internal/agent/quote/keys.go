package quote

import (
	"errors"
	"fmt"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
)

// Persistent handles. Both lie in the owner-controlled persistent range, in
// the block the TCG registry sets aside for endorsement-hierarchy objects.
const (
	// AKHandle is where the attestation key is made persistent; the verifier
	// pins the public key it sees there on first use.
	AKHandle tpm2.TPMHandle = 0x81010002
	// EKHandle is the TCG-assigned handle of the RSA-2048 EK. It is read when
	// a TPM has one; otherwise the EK is derived transiently from RSAEKTemplate.
	EKHandle tpm2.TPMHandle = 0x81010001
)

// AKTemplate is the attestation key: ECC P-256, restricted signing,
// ECDSA-SHA256, fixed to this TPM and to its parent, no authorization
// value. Restricted plus SignEncrypt is what lets it sign TPM2_Quote output
// and nothing supplied from outside.
var AKTemplate = tpm2.TPMTPublic{
	Type:    tpm2.TPMAlgECC,
	NameAlg: tpm2.TPMAlgSHA256,
	ObjectAttributes: tpm2.TPMAObject{
		FixedTPM:            true,
		FixedParent:         true,
		SensitiveDataOrigin: true,
		UserWithAuth:        true,
		NoDA:                true,
		Restricted:          true,
		SignEncrypt:         true,
	},
	Parameters: tpm2.NewTPMUPublicParms(tpm2.TPMAlgECC, &tpm2.TPMSECCParms{
		Scheme: tpm2.TPMTECCScheme{
			Scheme: tpm2.TPMAlgECDSA,
			Details: tpm2.NewTPMUAsymScheme(tpm2.TPMAlgECDSA,
				&tpm2.TPMSSigSchemeECDSA{HashAlg: tpm2.TPMAlgSHA256}),
		},
		CurveID: tpm2.TPMECCNistP256,
	}),
}

// AK is the loaded persistent attestation key.
type AK struct {
	// Handle is AKHandle.
	Handle tpm2.TPMHandle
	// Name is the TPM name of the key, needed to address it.
	Name tpm2.TPM2BName
	// Public is the key's public area.
	Public tpm2.TPMTPublic
	// PublicBytes is Public marshalled as TPMT_PUBLIC, the ak_pub wire value.
	PublicBytes []byte
}

// EnsureAK returns the AK at AKHandle, creating and persisting it on the
// first call for this TPM. The SRK it is created under is derived from the
// storage seed and flushed again; it does not need to persist.
func EnsureAK(tpm transport.TPM) (*AK, error) {
	ak, err := readAK(tpm)
	if err == nil {
		return ak, nil
	}
	if !errors.Is(err, tpm2.TPMRCHandle) {
		return nil, err
	}
	if err := createAK(tpm); err != nil {
		return nil, err
	}
	return readAK(tpm)
}

// readAK loads the public area at AKHandle; a TPM_RC_HANDLE error means the
// handle is empty.
func readAK(tpm transport.TPM) (*AK, error) {
	rsp, err := tpm2.ReadPublic{ObjectHandle: AKHandle}.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("read public 0x%x: %w", uint32(AKHandle), err)
	}
	pub, err := rsp.OutPublic.Contents()
	if err != nil {
		return nil, fmt.Errorf("decode public 0x%x: %w", uint32(AKHandle), err)
	}
	if err := matchesAKTemplate(pub); err != nil {
		return nil, fmt.Errorf("%w at 0x%x: %w", ErrForeignAK, uint32(AKHandle), err)
	}
	return &AK{Handle: AKHandle, Name: rsp.Name, Public: *pub, PublicBytes: tpm2.Marshal(pub)}, nil
}

// ErrForeignAK is returned when AKHandle holds a key that was not created
// from AKTemplate: reused or cloned TPM state, or another tool's key. The
// agent refuses to quote with it rather than adopt an unknown key; clearing
// the TPM state (a new VM) is the remedy.
var ErrForeignAK = errors.New("persistent object is not the vm-manager attestation key")

// matchesAKTemplate checks the immutable parts of a public area against
// AKTemplate: algorithm, name algorithm, attributes and the ECC parameters
// (curve, scheme, hash). The unique field is the key itself and differs.
func matchesAKTemplate(pub *tpm2.TPMTPublic) error {
	switch {
	case pub.Type != AKTemplate.Type:
		return fmt.Errorf("type %v, want %v", pub.Type, AKTemplate.Type)
	case pub.NameAlg != AKTemplate.NameAlg:
		return fmt.Errorf("name algorithm %v, want %v", pub.NameAlg, AKTemplate.NameAlg)
	case pub.ObjectAttributes != AKTemplate.ObjectAttributes:
		return fmt.Errorf("attributes %+v, want %+v", pub.ObjectAttributes, AKTemplate.ObjectAttributes)
	}
	got, err := pub.Parameters.ECCDetail()
	if err != nil {
		return fmt.Errorf("ecc parameters: %w", err)
	}
	want, _ := AKTemplate.Parameters.ECCDetail()
	switch {
	case got.CurveID != want.CurveID:
		return fmt.Errorf("curve %v, want %v", got.CurveID, want.CurveID)
	case got.Scheme.Scheme != want.Scheme.Scheme:
		return fmt.Errorf("scheme %v, want %v", got.Scheme.Scheme, want.Scheme.Scheme)
	}
	gotHash, err := got.Scheme.Details.ECDSA()
	if err != nil {
		return fmt.Errorf("scheme details: %w", err)
	}
	if gotHash.HashAlg != tpm2.TPMAlgSHA256 {
		return fmt.Errorf("scheme hash %v, want %v", gotHash.HashAlg, tpm2.TPMAlgSHA256)
	}
	return nil
}

// createAK creates the AK under a transient SRK and persists it at AKHandle.
func createAK(tpm transport.TPM) error {
	srk, err := tpm2.CreatePrimary{
		PrimaryHandle: tpm2.TPMRHOwner,
		InPublic:      tpm2.New2B(tpm2.ECCSRKTemplate),
	}.Execute(tpm)
	if err != nil {
		return fmt.Errorf("create srk: %w", err)
	}
	defer flush(tpm, srk.ObjectHandle)
	parent := tpm2.NamedHandle{Handle: srk.ObjectHandle, Name: srk.Name}

	created, err := tpm2.Create{ParentHandle: parent, InPublic: tpm2.New2B(AKTemplate)}.Execute(tpm)
	if err != nil {
		return fmt.Errorf("create ak: %w", err)
	}
	loaded, err := tpm2.Load{ParentHandle: parent, InPrivate: created.OutPrivate, InPublic: created.OutPublic}.Execute(tpm)
	if err != nil {
		return fmt.Errorf("load ak: %w", err)
	}
	defer flush(tpm, loaded.ObjectHandle)

	_, err = tpm2.EvictControl{
		Auth:             tpm2.TPMRHOwner,
		ObjectHandle:     tpm2.NamedHandle{Handle: loaded.ObjectHandle, Name: loaded.Name},
		PersistentHandle: AKHandle,
	}.Execute(tpm)
	if err != nil {
		return fmt.Errorf("persist ak at 0x%x: %w", uint32(AKHandle), err)
	}
	return nil
}

// Quote signs the sha256 PCR selection pcrs with the AK, nonce as qualifying
// data, and returns the marshalled TPMS_ATTEST and TPMT_SIGNATURE.
func (ak *AK) Quote(tpm transport.TPM, alg tpm2.TPMAlgID, pcrs []uint, nonce []byte) (attest, sig []byte, err error) {
	rsp, err := tpm2.Quote{
		SignHandle:     tpm2.NamedHandle{Handle: ak.Handle, Name: ak.Name},
		QualifyingData: tpm2.TPM2BData{Buffer: nonce},
		InScheme:       tpm2.TPMTSigScheme{Scheme: tpm2.TPMAlgNull},
		PCRSelect:      selection(alg, pcrs),
	}.Execute(tpm)
	if err != nil {
		return nil, nil, err
	}
	return rsp.Quoted.Bytes(), tpm2.Marshal(&rsp.Signature), nil
}

// EKPublic returns the endorsement key's TPMT_PUBLIC bytes: the object at
// EKHandle when the TPM persists one, otherwise the primary derived from
// RSAEKTemplate, flushed again after reading.
func EKPublic(tpm transport.TPM) ([]byte, error) {
	if rsp, err := (tpm2.ReadPublic{ObjectHandle: EKHandle}).Execute(tpm); err == nil {
		return rsp.OutPublic.Bytes(), nil
	} else if !errors.Is(err, tpm2.TPMRCHandle) {
		return nil, fmt.Errorf("read public 0x%x: %w", uint32(EKHandle), err)
	}
	ek, err := tpm2.CreatePrimary{
		PrimaryHandle: tpm2.TPMRHEndorsement,
		InPublic:      tpm2.New2B(tpm2.RSAEKTemplate),
	}.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("create ek: %w", err)
	}
	defer flush(tpm, ek.ObjectHandle)
	return ek.OutPublic.Bytes(), nil
}

// flush releases a transient object; a failure only leaks a slot until the
// next reboot and is not worth failing the quote over.
func flush(tpm transport.TPM, h tpm2.TPMHandle) {
	_, _ = tpm2.FlushContext{FlushHandle: h}.Execute(tpm)
}
