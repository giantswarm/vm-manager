package layersign

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testRoot = "add9ce6b00112233445566778899aabbccddeeff00112233445566778899138a"

type testKey struct {
	name   string
	signer crypto.Signer
	cert   *x509.Certificate
}

func newTestKey(t *testing.T, kind string) testKey {
	t.Helper()
	var (
		signer crypto.Signer
		err    error
	)
	switch kind {
	case "rsa":
		signer, err = rsa.GenerateKey(rand.Reader, 2048)
	case "p256":
		signer, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "p384":
		signer, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	}
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "layer signing test " + kind},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, signer.Public(), signer)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return testKey{name: kind, signer: signer, cert: cert}
}

func (k testKey) writePEM(t *testing.T, dir string) (keyPath, certPath string) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k.signer)
	require.NoError(t, err)
	keyPath, certPath = filepath.Join(dir, k.name+".key"), filepath.Join(dir, k.name+".crt")
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: k.cert.Raw}), 0o600))
	return keyPath, certPath
}

func parseSignedData(t *testing.T, der []byte) signedData {
	t.Helper()
	var ci contentInfo
	_, err := asn1.Unmarshal(der, &ci)
	require.NoError(t, err)
	require.True(t, ci.ContentType.Equal(oidSignedData))
	var sd signedData
	_, err = asn1.Unmarshal(ci.Content.Bytes, &sd)
	require.NoError(t, err)
	return sd
}

func TestSignContent(t *testing.T) {
	for _, kind := range []string{"rsa", "p256", "p384"} {
		t.Run(kind, func(t *testing.T) {
			k := newTestKey(t, kind)
			sig, err := signContent([]byte(testRoot), k.signer, k.cert)
			require.NoError(t, err)

			sd := parseSignedData(t, sig)
			require.True(t, sd.EncapContentInfo.EContentType.Equal(oidData), "id-data")
			require.Empty(t, sd.EncapContentInfo.EContent.FullBytes, "detached")
			require.Equal(t, k.cert.Raw, sd.Certificates.Bytes, "the signer certificate, no chain")
			require.Len(t, sd.SignerInfos, 1)
			require.Empty(t, sd.SignerInfos[0].SignedAttrs.FullBytes, "no signed attributes")
			require.Empty(t, sd.SignerInfos[0].UnsignedAttrs.FullBytes)

			again, err := signContent([]byte(testRoot), k.signer, k.cert)
			require.NoError(t, err)
			require.Equal(t, sig, again, "a re-sign is byte-identical")

			signedBy, err := verifyContent(sig, []byte(testRoot), []*x509.Certificate{newTestKey(t, "p256").cert, k.cert})
			require.NoError(t, err)
			require.Equal(t, k.cert, signedBy)

			_, err = verifyContent(sig, []byte(testRoot[1:]+"0"), []*x509.Certificate{k.cert})
			require.Error(t, err, "other content")
			_, err = verifyContent(sig, []byte(testRoot), []*x509.Certificate{newTestKey(t, kind).cert})
			require.ErrorContains(t, err, "none of the given certificates")

			opensslVerify(t, sig, k.cert)
		})
	}
}

// opensslVerify cross-checks a signature with openssl cms, when installed.
func opensslVerify(t *testing.T, sig []byte, cert *x509.Certificate) {
	t.Helper()
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Log("openssl not installed; no cross-check")
		return
	}
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, b, 0o600))
		return p
	}
	sigPath, content := write("sig", sig), write("content", []byte(testRoot))
	certPath := write("cert.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
	//nolint:gosec // G204: openssl from PATH, test files
	out, err := exec.Command(openssl, "cms", "-verify", "-binary", "-inform", "DER", "-in", sigPath,
		"-content", content, "-CAfile", certPath, "-purpose", "any", "-out", os.DevNull).CombinedOutput()
	require.NoError(t, err, string(out))
	//nolint:gosec // G204: openssl from PATH, test files
	out, err = exec.Command(openssl, "cms", "-cmsout", "-print", "-inform", "DER", "-in", sigPath).CombinedOutput()
	require.NoError(t, err, string(out))
	require.Regexp(t, `signedAttrs:\s+<ABSENT>`, string(out))
}

func TestVerifyContentRefusesOpenSSLDefaults(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl not installed")
	}
	k := newTestKey(t, "rsa")
	dir := t.TempDir()
	keyPath, certPath := k.writePEM(t, dir)
	content := filepath.Join(dir, "content")
	require.NoError(t, os.WriteFile(content, []byte(testRoot), 0o600))
	sign := func(args ...string) []byte {
		out := filepath.Join(dir, "sig")
		_ = os.Remove(out)
		//nolint:gosec // G204: openssl from PATH, test files
		cmd := exec.Command(openssl, append([]string{"cms", "-sign", "-binary", "-in", content, "-signer", certPath,
			"-inkey", keyPath, "-md", "sha256", "-outform", "DER", "-out", out}, args...)...)
		b, err := cmd.CombinedOutput()
		require.NoError(t, err, string(b))
		sig, err := os.ReadFile(out) //nolint:gosec // G304: a test file
		require.NoError(t, err)
		return sig
	}
	certs := []*x509.Certificate{k.cert}

	_, err = verifyContent(sign("-noattr"), []byte(testRoot), certs)
	require.NoError(t, err, "what the contract names: openssl cms -sign -binary -noattr")

	_, err = verifyContent(sign(), []byte(testRoot), certs)
	require.ErrorContains(t, err, "smimeCapabilities")

	_, err = verifyContent(sign("-noattr", "-nodetach"), []byte(testRoot), certs)
	require.ErrorContains(t, err, "detached")
}

func TestVerifyContentRefusesRSAPSS(t *testing.T) {
	k := newTestKey(t, "rsa")
	sig, err := signContent([]byte(testRoot), k.signer, k.cert)
	require.NoError(t, err)
	var ci contentInfo
	_, err = asn1.Unmarshal(sig, &ci)
	require.NoError(t, err)
	sd := parseSignedData(t, sig)
	sd.SignerInfos[0].SignatureAlgorithm = pkix.AlgorithmIdentifier{Algorithm: oidRSAPSS}
	sdDER, err := asn1.Marshal(sd)
	require.NoError(t, err)
	pss, err := asn1.Marshal(contentInfo{ContentType: oidSignedData, Content: asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: sdDER}})
	require.NoError(t, err)

	_, err = verifyContent(pss, []byte(testRoot), []*x509.Certificate{k.cert})
	require.ErrorContains(t, err, "RSA-PSS")
}

func TestParsePrivateKeyRefusesRSAPSS(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := asn1.Marshal(struct {
		Version    int
		Algorithm  pkix.AlgorithmIdentifier
		PrivateKey []byte
	}{0, pkix.AlgorithmIdentifier{Algorithm: oidRSAPSS}, x509.MarshalPKCS1PrivateKey(k)})
	require.NoError(t, err)

	_, err = ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	require.ErrorIs(t, err, errRSAPSS)
}

func TestParsePrivateKey(t *testing.T) {
	rsaKey := newTestKey(t, "rsa").signer.(*rsa.PrivateKey)
	ecKey := newTestKey(t, "p256").signer.(*ecdsa.PrivateKey)
	ecDER, err := x509.MarshalECPrivateKey(ecKey)
	require.NoError(t, err)
	p224, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	require.NoError(t, err)
	p224DER, err := x509.MarshalECPrivateKey(p224)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		pem     *pem.Block
		wantErr string
	}{
		"PKCS#1": {pem: &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}},
		"SEC 1":  {pem: &pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER}},
		"P-224":  {pem: &pem.Block{Type: "EC PRIVATE KEY", Bytes: p224DER}, wantErr: "P-224"},
	} {
		t.Run(name, func(t *testing.T) {
			signer, err := ParsePrivateKey(append([]byte("comment\n"), pem.EncodeToMemory(tc.pem)...))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, signer)
		})
	}
}

func TestECDSADER(t *testing.T) {
	k := newTestKey(t, "p384")
	pub := k.signer.Public().(*ecdsa.PublicKey)
	der, err := k.signer.Sign(rand.Reader, bytes.Repeat([]byte{1}, 32), crypto.SHA256)
	require.NoError(t, err)
	require.Equal(t, der, ecdsaDER(der, pub), "DER stays DER")

	var parsed struct{ R, S *big.Int }
	_, err = asn1.Unmarshal(der, &parsed)
	require.NoError(t, err)
	raw := make([]byte, 96)
	parsed.R.FillBytes(raw[:48])
	parsed.S.FillBytes(raw[48:])
	require.Equal(t, der, ecdsaDER(raw, pub), "a KMS's r‖s becomes DER")
}
