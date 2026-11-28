// Package layercerttest makes self-signed layer certificates for tests.
package layercerttest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Cert is a self-signed certificate with its key.
type Cert struct {
	// PEM is the certificate in canonical PEM (layercert.Normalize's form).
	PEM string
	// Digest is the lowercase hex sha256 of the certificate's DER.
	Digest string
	// KeyPEM is the private key, for tests that pass one where a
	// certificate belongs.
	KeyPEM string
}

// New returns a fresh P-256 certificate named cn.
func New(t testing.TB, cn string) Cert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	sum := sha256.Sum256(der)
	return Cert{
		PEM:    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		Digest: hex.EncodeToString(sum[:]),
		KeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
	}
}
