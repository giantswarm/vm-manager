package layersign

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
)

// errRSAPSS: the kernel's PKCS#7 parser takes RSA PKCS#1 v1.5 and ECDSA only.
var errRSAPSS = errors.New("RSA-PSS keys are not allowed: the kernel checks dm-verity signatures made with RSA PKCS#1 v1.5 or ECDSA P-256/P-384")

// ParsePrivateKey reads the first private key of a PEM file: PKCS#8
// ("PRIVATE KEY"), PKCS#1 ("RSA PRIVATE KEY") or SEC 1 ("EC PRIVATE KEY").
// A key in a KMS is not read from a file: its crypto.Signer goes into
// SignOptions directly.
func ParsePrivateKey(data []byte) (crypto.Signer, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, errors.New("no PEM private key found")
		}
		var (
			key any
			err error
		)
		switch block.Type {
		case "PRIVATE KEY":
			if isPKCS8RSAPSS(block.Bytes) {
				return nil, errRSAPSS
			}
			key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "RSA PRIVATE KEY":
			key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(block.Bytes)
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", block.Type, err)
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("a %T cannot sign", key)
		}
		if err := checkPublicKey(signer.Public()); err != nil {
			return nil, err
		}
		return signer, nil
	}
}

// ParseCertificates reads every certificate of PEM data ("CERTIFICATE"
// blocks), or one DER certificate.
func ParseCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := parseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		cert, err := parseCertificate(data)
		if err != nil {
			return nil, fmt.Errorf("no PEM certificate found, and not a DER one: %w", err)
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

func parseCertificate(der []byte) (*x509.Certificate, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	if cert.PublicKeyAlgorithm == x509.UnknownPublicKeyAlgorithm && isSPKIRSAPSS(cert.RawSubjectPublicKeyInfo) {
		return nil, fmt.Errorf("certificate %s: %w", cert.Subject, errRSAPSS)
	}
	if err := checkPublicKey(cert.PublicKey); err != nil {
		return nil, fmt.Errorf("certificate %s: %w", cert.Subject, err)
	}
	return cert, nil
}

// checkPublicKey allows the key types the kernel verifies dm-verity
// signatures with.
func checkPublicKey(pub crypto.PublicKey) error {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return nil
	case *ecdsa.PublicKey:
		if k.Curve == elliptic.P256() || k.Curve == elliptic.P384() {
			return nil
		}
		return fmt.Errorf("ECDSA curve %s is not allowed (P-256 or P-384)", k.Curve.Params().Name)
	default:
		return fmt.Errorf("%T keys are not allowed (RSA or ECDSA P-256/P-384)", pub)
	}
}

func isPKCS8RSAPSS(der []byte) bool {
	var pkcs8 struct {
		Version    int
		Algorithm  pkix.AlgorithmIdentifier
		PrivateKey []byte
	}
	if _, err := asn1.Unmarshal(der, &pkcs8); err != nil {
		return false
	}
	return pkcs8.Algorithm.Algorithm.Equal(oidRSAPSS)
}

func isSPKIRSAPSS(der []byte) bool {
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(der, &spki); err != nil {
		return false
	}
	return spki.Algorithm.Algorithm.Equal(oidRSAPSS)
}

// CertificateFor returns the certificate of certs whose public key is
// signer's, or nil.
func CertificateFor(signer crypto.Signer, certs []*x509.Certificate) *x509.Certificate {
	for _, c := range certs {
		if k, ok := c.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); ok && k.Equal(signer.Public()) {
			return c
		}
	}
	return nil
}
