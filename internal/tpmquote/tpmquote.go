// Package tpmquote parses and cryptographically verifies TPM 2.0 quotes the
// way the guest agent posts them to the IMDS: the marshalled TPMS_ATTEST
// that TPM2_Quote produced, the TPMT_SIGNATURE over it and the TPMT_PUBLIC
// of the attestation key (AK). The package is pure: it holds no state, talks
// to no TPM and knows no policy; internal/attest applies nonces, AK pinning
// and the image policy on top of it.
//
// What the agent must send, byte for byte:
//
//   - quote: the TPMS_ATTEST bytes (TPM2B_ATTEST.Bytes(), no size prefix),
//     the exact bytes the TPM signed;
//   - signature: tpm2.Marshal of the TPMT_SIGNATURE;
//   - ak_pub: tpm2.Marshal of the AK's TPMT_PUBLIC (a TPM2B_PUBLIC wrapper is
//     accepted too; both yield the same Fingerprint);
//   - one PCR bank in the selection, and the nonce, hex-decoded, as the
//     qualifying data.
package tpmquote

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/google/go-tpm/tpm2"

	// Register the hashes a signature may name; crypto.Hash.New panics otherwise.
	_ "crypto/sha512"
)

// Every failure wraps one of these so callers can tell the checks apart.
var (
	// ErrMalformed is a quote, signature or public area go-tpm cannot decode.
	ErrMalformed = errors.New("malformed quote")
	// ErrMagic is an attestation structure not produced by a TPM.
	ErrMagic = errors.New("quote magic is not TPM_GENERATED_VALUE")
	// ErrType is an attestation structure that is not a TPM2_Quote result.
	ErrType = errors.New("attestation is not a quote")
	// ErrAKAttributes is an attestation key that is not a fixed, restricted signing key.
	ErrAKAttributes = errors.New("attestation key attributes")
	// ErrAKAlgorithm is an attestation key that is neither ECC P-256 nor RSA 2048.
	ErrAKAlgorithm = errors.New("unsupported attestation key")
	// ErrSignature is a signature that does not verify with the presented AK.
	ErrSignature = errors.New("signature does not verify")
	// ErrNonce is qualifying data that does not equal the nonce.
	ErrNonce = errors.New("nonce mismatch")
	// ErrPCRDigest is a PCR digest that does not match the PCR values sent.
	ErrPCRDigest = errors.New("pcr digest mismatch")
	// ErrPCRValue is a PCR value missing or of the wrong length for the bank.
	ErrPCRValue = errors.New("pcr value")
	// ErrBank is a PCR bank the verifier cannot hash.
	ErrBank = errors.New("unsupported pcr bank")
)

// AKTemplate is the attestation key the guest agent creates and the checks
// in Parse expect: an ECC P-256 restricted signing key (ECDSA-SHA256,
// nameAlg SHA256) fixed to the TPM. Any fixedTPM+fixedParent+
// sensitiveDataOrigin+restricted+sign, non-decrypt ECC P-256 or RSA 2048 key
// passes; this is the reference template.
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
			Scheme:  tpm2.TPMAlgECDSA,
			Details: tpm2.NewTPMUAsymScheme(tpm2.TPMAlgECDSA, &tpm2.TPMSSigSchemeECDSA{HashAlg: tpm2.TPMAlgSHA256}),
		},
		CurveID: tpm2.TPMECCNistP256,
	}),
}

// Quote is a parsed TPM2_Quote result. Parse fills it; VerifySignature,
// VerifyNonce and VerifyPCRs judge it.
type Quote struct {
	// Attest is the TPMS_ATTEST exactly as received; the signature covers it.
	Attest []byte
	// Signature is the decoded TPMT_SIGNATURE.
	Signature tpm2.TPMTSignature
	// Public is the AK's public area; AKFingerprint is Fingerprint(Public).
	Public        tpm2.TPMTPublic
	AKFingerprint string
	// ExtraData is the qualifying data the caller passed to TPM2_Quote, the
	// nonce in this protocol.
	ExtraData []byte
	// Bank names the quoted PCR bank ("sha256"); PCRs are the selected
	// indexes in ascending order; PCRDigest is the TPM's digest over their
	// values, computed with Hash, the hash of the signing scheme.
	Bank      string
	PCRs      []int
	PCRDigest []byte
	Hash      crypto.Hash
}

// Parse decodes quote, signature and AK public area and checks everything
// that needs no PCR values and no nonce: the TPM magic, the attestation
// type, a single PCR bank, and the AK's algorithm and attributes.
func Parse(quote, sig, akPub []byte) (*Quote, error) {
	if len(quote) < 4 {
		return nil, fmt.Errorf("%w: attest is %d bytes", ErrMalformed, len(quote))
	}
	if magic := binary.BigEndian.Uint32(quote[:4]); magic != uint32(tpm2.TPMGeneratedValue) {
		return nil, fmt.Errorf("%w: got 0x%08x", ErrMagic, magic)
	}
	att, err := tpm2.Unmarshal[tpm2.TPMSAttest](quote)
	if err != nil {
		return nil, fmt.Errorf("%w: attest: %v", ErrMalformed, err)
	}
	if att.Type != tpm2.TPMSTAttestQuote {
		return nil, fmt.Errorf("%w: type 0x%04x", ErrType, uint16(att.Type))
	}
	info, err := att.Attested.Quote()
	if err != nil {
		return nil, fmt.Errorf("%w: quote info: %v", ErrMalformed, err)
	}
	bank, pcrs, err := decodeSelection(info.PCRSelect)
	if err != nil {
		return nil, err
	}
	signature, err := tpm2.Unmarshal[tpm2.TPMTSignature](sig)
	if err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrMalformed, err)
	}
	pub, err := parsePublic(akPub)
	if err != nil {
		return nil, err
	}
	if err := checkAK(pub); err != nil {
		return nil, err
	}
	hash, err := signatureHash(pub, signature)
	if err != nil {
		return nil, err
	}
	return &Quote{
		Attest:        quote,
		Signature:     *signature,
		Public:        *pub,
		AKFingerprint: Fingerprint(pub),
		ExtraData:     att.ExtraData.Buffer,
		Bank:          bank,
		PCRs:          pcrs,
		PCRDigest:     info.PCRDigest.Buffer,
		Hash:          hash,
	}, nil
}

// Fingerprint identifies an AK: the hex sha256 of its marshalled TPMT_PUBLIC.
func Fingerprint(pub *tpm2.TPMTPublic) string {
	sum := sha256.Sum256(tpm2.Marshal(pub))
	return hex.EncodeToString(sum[:])
}

// VerifySignature checks q.Signature over q.Attest with q.Public: ECDSA for
// ECC keys, RSASSA (PKCS#1 v1.5) or RSAPSS for RSA keys.
func VerifySignature(q *Quote) error {
	pub, err := tpm2.Pub(q.Public)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAKAlgorithm, err)
	}
	h := q.Hash.New()
	h.Write(q.Attest)
	digest := h.Sum(nil)

	switch q.Signature.SigAlg {
	case tpm2.TPMAlgECDSA:
		key, ok := pub.(*ecdsa.PublicKey)
		s, err := q.Signature.Signature.ECDSA()
		if !ok || err != nil {
			return fmt.Errorf("%w: ecdsa signature with a non-ecc key", ErrSignature)
		}
		r, ss := new(big.Int).SetBytes(s.SignatureR.Buffer), new(big.Int).SetBytes(s.SignatureS.Buffer)
		if !ecdsa.Verify(key, digest, r, ss) {
			return ErrSignature
		}
	case tpm2.TPMAlgRSASSA:
		key, ok := pub.(*rsa.PublicKey)
		s, err := q.Signature.Signature.RSASSA()
		if !ok || err != nil {
			return fmt.Errorf("%w: rsassa signature with a non-rsa key", ErrSignature)
		}
		if err := rsa.VerifyPKCS1v15(key, q.Hash, digest, s.Sig.Buffer); err != nil {
			return ErrSignature
		}
	case tpm2.TPMAlgRSAPSS:
		key, ok := pub.(*rsa.PublicKey)
		s, err := q.Signature.Signature.RSAPSS()
		if !ok || err != nil {
			return fmt.Errorf("%w: rsapss signature with a non-rsa key", ErrSignature)
		}
		if err := rsa.VerifyPSS(key, q.Hash, digest, s.Sig.Buffer, nil); err != nil {
			return ErrSignature
		}
	default:
		return fmt.Errorf("%w: scheme 0x%04x", ErrSignature, uint16(q.Signature.SigAlg))
	}
	return nil
}

// VerifyNonce checks that the quote's qualifying data is the hex-decoded
// nonce.
func VerifyNonce(extraData []byte, nonce string) error {
	want, err := hex.DecodeString(nonce)
	if err != nil {
		return fmt.Errorf("%w: nonce is not hex: %v", ErrNonce, err)
	}
	if len(want) == 0 || subtle.ConstantTimeCompare(extraData, want) != 1 {
		return ErrNonce
	}
	return nil
}

// PCRDigest computes what TPM2_Quote puts into pcrDigest: h over the
// concatenation of the selected PCR values in ascending index order. Every
// selected index needs a value.
func PCRDigest(h crypto.Hash, selection []int, values map[int][]byte) ([]byte, error) {
	idx := append([]int(nil), selection...)
	sort.Ints(idx)
	d := h.New()
	for _, i := range idx {
		v, ok := values[i]
		if !ok || len(v) == 0 {
			return nil, fmt.Errorf("%w: no value for pcr %d", ErrPCRValue, i)
		}
		d.Write(v)
	}
	return d.Sum(nil), nil
}

// VerifyPCRs checks that values (index -> digest of q.Bank) reproduce
// q.PCRDigest. Values for indexes the quote does not select are ignored.
func VerifyPCRs(q *Quote, values map[int][]byte) error {
	bank, err := BankHash(q.Bank)
	if err != nil {
		return err
	}
	for _, i := range q.PCRs {
		if v, ok := values[i]; ok && len(v) != bank.Size() {
			return fmt.Errorf("%w: pcr %d has %d bytes, %s needs %d", ErrPCRValue, i, len(v), q.Bank, bank.Size())
		}
	}
	got, err := PCRDigest(q.Hash, q.PCRs, values)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(got, q.PCRDigest) != 1 {
		return ErrPCRDigest
	}
	return nil
}

// BankHash maps a bank name ("sha256") to its hash.
func BankHash(bank string) (crypto.Hash, error) {
	for h, name := range bankNames {
		if name == bank {
			return h, nil
		}
	}
	return 0, fmt.Errorf("%w: %q", ErrBank, bank)
}

var bankNames = map[crypto.Hash]string{
	crypto.SHA1:   "sha1",
	crypto.SHA256: "sha256",
	crypto.SHA384: "sha384",
	crypto.SHA512: "sha512",
}

// decodeSelection requires exactly one bank and returns its name and the
// selected indexes in ascending order.
func decodeSelection(sel tpm2.TPMLPCRSelection) (string, []int, error) {
	var (
		bank string
		pcrs []int
	)
	for _, s := range sel.PCRSelections {
		var idx []int
		for byteN, b := range s.PCRSelect {
			for bit := 0; bit < 8; bit++ {
				if b&(1<<bit) != 0 {
					idx = append(idx, byteN*8+bit)
				}
			}
		}
		if len(idx) == 0 {
			continue
		}
		if bank != "" {
			return "", nil, fmt.Errorf("%w: quote selects more than one pcr bank", ErrMalformed)
		}
		h, err := s.Hash.Hash()
		if err != nil {
			return "", nil, fmt.Errorf("%w: pcr bank 0x%04x", ErrBank, uint16(s.Hash))
		}
		name, ok := bankNames[h]
		if !ok {
			return "", nil, fmt.Errorf("%w: %v", ErrBank, h)
		}
		bank, pcrs = name, idx
	}
	if bank == "" {
		return "", nil, fmt.Errorf("%w: quote selects no pcr", ErrMalformed)
	}
	return bank, pcrs, nil
}

// parsePublic accepts a TPMT_PUBLIC or a TPM2B_PUBLIC.
func parsePublic(akPub []byte) (*tpm2.TPMTPublic, error) {
	pub, err := tpm2.Unmarshal[tpm2.TPMTPublic](akPub)
	if err == nil {
		return pub, nil
	}
	wrapped, err2 := tpm2.Unmarshal[tpm2.TPM2BPublic](akPub)
	if err2 != nil {
		return nil, fmt.Errorf("%w: ak_pub: %v", ErrMalformed, err)
	}
	pub, err2 = wrapped.Contents()
	if err2 != nil {
		return nil, fmt.Errorf("%w: ak_pub: %v", ErrMalformed, err2)
	}
	return pub, nil
}

// checkAK requires a key that only ever signs TPM-internal data: fixed to
// this TPM and parent, TPM-generated, restricted, signing, not decrypting;
// ECC P-256 with ECDSA or RSA 2048 with RSASSA or RSAPSS.
func checkAK(pub *tpm2.TPMTPublic) error {
	a := pub.ObjectAttributes
	var missing []string
	for name, set := range map[string]bool{
		"fixedTPM": a.FixedTPM, "fixedParent": a.FixedParent, "sensitiveDataOrigin": a.SensitiveDataOrigin,
		"restricted": a.Restricted, "sign": a.SignEncrypt,
	} {
		if !set {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	switch {
	case len(missing) > 0:
		return fmt.Errorf("%w: %v not set", ErrAKAttributes, missing)
	case a.Decrypt:
		return fmt.Errorf("%w: decrypt is set", ErrAKAttributes)
	}

	switch pub.Type {
	case tpm2.TPMAlgECC:
		p, err := pub.Parameters.ECCDetail()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		if p.CurveID != tpm2.TPMECCNistP256 {
			return fmt.Errorf("%w: ecc curve 0x%04x, want P-256", ErrAKAlgorithm, uint16(p.CurveID))
		}
		if p.Scheme.Scheme != tpm2.TPMAlgECDSA {
			return fmt.Errorf("%w: ecc scheme 0x%04x, want ECDSA", ErrAKAlgorithm, uint16(p.Scheme.Scheme))
		}
	case tpm2.TPMAlgRSA:
		p, err := pub.Parameters.RSADetail()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		if p.KeyBits != 2048 {
			return fmt.Errorf("%w: rsa %d bits, want 2048", ErrAKAlgorithm, p.KeyBits)
		}
		if s := p.Scheme.Scheme; s != tpm2.TPMAlgRSASSA && s != tpm2.TPMAlgRSAPSS {
			return fmt.Errorf("%w: rsa scheme 0x%04x, want RSASSA or RSAPSS", ErrAKAlgorithm, uint16(s))
		}
	default:
		return fmt.Errorf("%w: type 0x%04x", ErrAKAlgorithm, uint16(pub.Type))
	}
	return nil
}

// signatureHash is the hash named in the signature, which must suit the key.
func signatureHash(pub *tpm2.TPMTPublic, sig *tpm2.TPMTSignature) (crypto.Hash, error) {
	var alg tpm2.TPMIAlgHash
	switch sig.SigAlg {
	case tpm2.TPMAlgECDSA:
		if pub.Type != tpm2.TPMAlgECC {
			return 0, fmt.Errorf("%w: ecdsa signature from a non-ecc key", ErrSignature)
		}
		s, err := sig.Signature.ECDSA()
		if err != nil {
			return 0, fmt.Errorf("%w: signature: %v", ErrMalformed, err)
		}
		alg = s.Hash
	case tpm2.TPMAlgRSASSA, tpm2.TPMAlgRSAPSS:
		if pub.Type != tpm2.TPMAlgRSA {
			return 0, fmt.Errorf("%w: rsa signature from a non-rsa key", ErrSignature)
		}
		s, err := sig.Signature.RSASSA()
		if sig.SigAlg == tpm2.TPMAlgRSAPSS {
			s, err = sig.Signature.RSAPSS()
		}
		if err != nil {
			return 0, fmt.Errorf("%w: signature: %v", ErrMalformed, err)
		}
		alg = s.Hash
	default:
		return 0, fmt.Errorf("%w: scheme 0x%04x", ErrSignature, uint16(sig.SigAlg))
	}
	h, err := alg.Hash()
	if err != nil || !h.Available() {
		return 0, fmt.Errorf("%w: signature hash 0x%04x", ErrSignature, uint16(alg))
	}
	return h, nil
}
