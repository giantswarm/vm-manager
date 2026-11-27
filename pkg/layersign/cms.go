package layersign

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"

	"golang.org/x/crypto/cryptobyte"
	cbasn1 "golang.org/x/crypto/cryptobyte/asn1"
)

// The signature is what the kernel's dm-verity signature check
// (dm_verity.require_signatures=1) accepts: CMS SignedData, DER, detached,
// content type id-data, SHA-256, no signed attributes (openssl's smimeCapabilities
// attribute makes the kernel refuse the key with EKEYREJECTED), the signer
// certificate embedded without a chain, RSA PKCS#1 v1.5 or ECDSA with a DER
// Ecdsa-Sig-Value. Equivalent to `openssl cms -sign -binary -noattr -nosmimecap
// -outform DER -md sha256`.

var (
	oidData              = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidSignedData        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidSHA256            = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidRSAEncryption     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidRSAPSS            = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 10}
	oidECDSAWithSHA256   = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidSMIMECapabilities = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 15}
)

type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"explicit,tag:0"`
}

type signedData struct {
	Version          int
	DigestAlgorithms []pkix.AlgorithmIdentifier `asn1:"set"`
	EncapContentInfo encapContentInfo
	Certificates     asn1.RawValue `asn1:"optional,tag:0"`
	CRLs             asn1.RawValue `asn1:"optional,tag:1"`
	SignerInfos      []signerInfo  `asn1:"set"`
}

type encapContentInfo struct {
	EContentType asn1.ObjectIdentifier
	EContent     asn1.RawValue `asn1:"optional,explicit,tag:0"`
}

type issuerAndSerialNumber struct {
	Issuer       asn1.RawValue
	SerialNumber *big.Int
}

type signerInfo struct {
	Version            int
	SID                issuerAndSerialNumber
	DigestAlgorithm    pkix.AlgorithmIdentifier
	SignedAttrs        asn1.RawValue `asn1:"optional,tag:0"`
	SignatureAlgorithm pkix.AlgorithmIdentifier
	Signature          []byte
	UnsignedAttrs      asn1.RawValue `asn1:"optional,tag:1"`
}

// signContent returns the detached CMS signature of content by signer, whose
// public key is cert's.
func signContent(content []byte, signer crypto.Signer, cert *x509.Certificate) ([]byte, error) {
	sigAlg, err := signatureAlgorithm(signer.Public())
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	// An ECDSA key of crypto/ecdsa signs deterministically (RFC 6979) without
	// a random source, so a re-sign gives the same bytes; RSA PKCS#1 v1.5 is
	// deterministic anyway, and a KMS signer ignores the source.
	var rnd = rand.Reader
	if _, ok := signer.(*ecdsa.PrivateKey); ok {
		rnd = nil
	}
	sig, err := signer.Sign(rnd, digest[:], crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	if pub, ok := signer.Public().(*ecdsa.PublicKey); ok {
		sig = ecdsaDER(sig, pub)
	}
	if err := verifyWithKey(cert.PublicKey, sigAlg.Algorithm, digest[:], sig); err != nil {
		return nil, fmt.Errorf("the signature does not verify against the certificate (key and certificate do not match?): %w", err)
	}

	sd := signedData{
		Version:          1,
		DigestAlgorithms: []pkix.AlgorithmIdentifier{{Algorithm: oidSHA256}},
		EncapContentInfo: encapContentInfo{EContentType: oidData},
		Certificates:     asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: cert.Raw},
		SignerInfos: []signerInfo{{
			Version:            1,
			SID:                issuerAndSerialNumber{Issuer: asn1.RawValue{FullBytes: cert.RawIssuer}, SerialNumber: cert.SerialNumber},
			DigestAlgorithm:    pkix.AlgorithmIdentifier{Algorithm: oidSHA256},
			SignatureAlgorithm: sigAlg,
			Signature:          sig,
		}},
	}
	sdDER, err := asn1.Marshal(sd)
	if err != nil {
		return nil, fmt.Errorf("encode SignedData: %w", err)
	}
	return asn1.Marshal(contentInfo{
		ContentType: oidSignedData,
		Content:     asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: sdDER},
	})
}

// verifyContent checks the detached CMS signature der over content by the
// kernel's rules and returns the certificate of certs that signed it.
func verifyContent(der, content []byte, certs []*x509.Certificate) (*x509.Certificate, error) {
	var ci contentInfo
	rest, err := asn1.Unmarshal(der, &ci)
	if err != nil {
		return nil, fmt.Errorf("parse CMS: %w", err)
	}
	if len(rest) != 0 {
		return nil, errors.New("parse CMS: trailing data")
	}
	if !ci.ContentType.Equal(oidSignedData) {
		return nil, fmt.Errorf("CMS content type %v is not SignedData", ci.ContentType)
	}
	var sd signedData
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil {
		return nil, fmt.Errorf("parse SignedData: %w", err)
	}
	if !sd.EncapContentInfo.EContentType.Equal(oidData) {
		return nil, fmt.Errorf("content type %v: the kernel accepts id-data only", sd.EncapContentInfo.EContentType)
	}
	if len(sd.EncapContentInfo.EContent.FullBytes) != 0 {
		return nil, errors.New("the signature embeds its content: the kernel accepts detached signatures only")
	}
	if len(sd.SignerInfos) == 0 {
		return nil, errors.New("the signature has no signer")
	}
	digest := sha256.Sum256(content)
	var signedBy *x509.Certificate
	for _, si := range sd.SignerInfos {
		if len(si.SignedAttrs.FullBytes) != 0 {
			if hasAttribute(si.SignedAttrs.Bytes, oidSMIMECapabilities) {
				return nil, errors.New("the signature carries an smimeCapabilities attribute, which the kernel rejects (EKEYREJECTED)")
			}
			return nil, errors.New("the signature carries signed attributes; a layer signature has none")
		}
		if !si.DigestAlgorithm.Algorithm.Equal(oidSHA256) {
			return nil, fmt.Errorf("digest algorithm %v is not SHA-256", si.DigestAlgorithm.Algorithm)
		}
		cert := findSigner(si.SID, certs)
		if cert == nil {
			return nil, fmt.Errorf("signed by serial %s of %s, which none of the given certificates is", si.SID.SerialNumber.Text(16), issuerString(si.SID.Issuer.FullBytes))
		}
		if err := verifyWithKey(cert.PublicKey, si.SignatureAlgorithm.Algorithm, digest[:], si.Signature); err != nil {
			return nil, fmt.Errorf("signature of %s: %w", cert.Subject, err)
		}
		signedBy = cert
	}
	return signedBy, nil
}

// signatureAlgorithm is the SignerInfo signature algorithm for pub, the ones
// the kernel's PKCS#7 parser accepts.
func signatureAlgorithm(pub crypto.PublicKey) (pkix.AlgorithmIdentifier, error) {
	if err := checkPublicKey(pub); err != nil {
		return pkix.AlgorithmIdentifier{}, err
	}
	if _, ok := pub.(*rsa.PublicKey); ok {
		return pkix.AlgorithmIdentifier{Algorithm: oidRSAEncryption, Parameters: asn1.NullRawValue}, nil
	}
	return pkix.AlgorithmIdentifier{Algorithm: oidECDSAWithSHA256}, nil
}

func verifyWithKey(pub crypto.PublicKey, alg asn1.ObjectIdentifier, digest, sig []byte) error {
	switch {
	case alg.Equal(oidRSAPSS):
		return errors.New("RSA-PSS signatures are not allowed")
	case alg.Equal(oidRSAEncryption):
		key, ok := pub.(*rsa.PublicKey)
		if !ok {
			return errors.New("an RSA signature from a non-RSA certificate")
		}
		return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest, sig)
	case alg.Equal(oidECDSAWithSHA256):
		key, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("an ECDSA signature from a non-ECDSA certificate")
		}
		if !ecdsa.VerifyASN1(key, digest, sig) {
			return errors.New("ECDSA signature does not verify")
		}
		return nil
	default:
		return fmt.Errorf("signature algorithm %v is not allowed (rsaEncryption or ecdsa-with-SHA256)", alg)
	}
}

// ecdsaDER encodes a raw r‖s ECDSA signature (what a KMS such as Azure Key
// Vault returns) as the DER Ecdsa-Sig-Value the kernel parses; a signature
// that is DER already is returned as it is.
func ecdsaDER(sig []byte, pub *ecdsa.PublicKey) []byte {
	size := (pub.Curve.Params().BitSize + 7) / 8
	if len(sig) != 2*size || sig[0] == 0x30 && isDERSignature(sig) {
		return sig
	}
	var b cryptobyte.Builder
	b.AddASN1(cbasn1.SEQUENCE, func(b *cryptobyte.Builder) {
		b.AddASN1BigInt(new(big.Int).SetBytes(sig[:size]))
		b.AddASN1BigInt(new(big.Int).SetBytes(sig[size:]))
	})
	der, err := b.Bytes()
	if err != nil {
		return sig
	}
	return der
}

func isDERSignature(sig []byte) bool {
	var inner cryptobyte.String
	s := cryptobyte.String(sig)
	r, t := new(big.Int), new(big.Int)
	return s.ReadASN1(&inner, cbasn1.SEQUENCE) && s.Empty() &&
		inner.ReadASN1Integer(r) && inner.ReadASN1Integer(t) && inner.Empty()
}

func findSigner(sid issuerAndSerialNumber, certs []*x509.Certificate) *x509.Certificate {
	for _, c := range certs {
		if bytes.Equal(c.RawIssuer, sid.Issuer.FullBytes) && c.SerialNumber.Cmp(sid.SerialNumber) == 0 {
			return c
		}
	}
	return nil
}

func hasAttribute(attrs []byte, oid asn1.ObjectIdentifier) bool {
	for len(attrs) > 0 {
		var attr struct {
			Type   asn1.ObjectIdentifier
			Values asn1.RawValue `asn1:"set"`
		}
		rest, err := asn1.Unmarshal(attrs, &attr)
		if err != nil {
			return false
		}
		if attr.Type.Equal(oid) {
			return true
		}
		attrs = rest
	}
	return false
}

func issuerString(raw []byte) string {
	var rdn pkix.RDNSequence
	if _, err := asn1.Unmarshal(raw, &rdn); err != nil {
		return "an unparsable issuer"
	}
	var name pkix.Name
	name.FillFromRDNSequence(&rdn)
	return name.String()
}
