package layercert_test

import (
	"encoding/pem"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/layercert"
	"github.com/giantswarm/vm-manager/internal/layercert/layercerttest"
)

// newCert returns a certificate's PEM, its DER digest and its key's PEM.
func newCert(t *testing.T, cn string) (certPEM, digest, keyPEM string) {
	c := layercerttest.New(t, cn)
	return c.PEM, c.Digest, c.KeyPEM
}

func TestNormalizeKeepsOrderAndCanonicalForm(t *testing.T) {
	a, da, _ := newCert(t, "a")
	b, db, _ := newCert(t, "b")
	c, dc, _ := newCert(t, "c")

	got, err := layercert.Normalize([]string{
		"\n  " + strings.ReplaceAll(c, "\n", "\r\n") + "\n",
		a + "\n\n" + b,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{c, a, b}, got, "caller's order, one canonical certificate per element")

	digests, err := layercert.Digests(got)
	require.NoError(t, err)
	assert.Equal(t, []string{dc, da, db}, digests)

	none, err := layercert.Normalize(nil)
	require.NoError(t, err)
	assert.Nil(t, none)
}

func TestNormalizeRejects(t *testing.T) {
	a, _, key := newCert(t, "a")
	b, _, _ := newCert(t, "b")
	block := func(typ string, b []byte) string { return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b})) }
	withHeader := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: []byte{1}}))
	many := make([]string, layercert.MaxCertificates+1)
	for i := range many {
		many[i], _, _ = newCert(t, "many")
	}

	for _, tc := range []struct {
		name  string
		items []string
		want  string
	}{
		{name: "private key", items: []string{a, key}, want: `layer certificate 1: block 0 is a "EC PRIVATE KEY" block, want "CERTIFICATE"`},
		{name: "key after certificate", items: []string{a + key}, want: "layer certificate 0: block 1 is a"},
		{name: "public key", items: []string{block("PUBLIC KEY", []byte{1})}, want: `"PUBLIC KEY" block`},
		{name: "garbage", items: []string{"not a certificate"}, want: "layer certificate 0: text outside a PEM block after block 0"},
		{name: "text before", items: []string{"subject=a\n" + a}, want: "text outside a PEM block"},
		{name: "text after", items: []string{a + "trailer"}, want: "text outside a PEM block after block 1"},
		{name: "broken pem", items: []string{"-----BEGIN CERTIFICATE-----\nAAAA"}, want: "block 0 is not valid PEM"},
		{name: "not x509", items: []string{block("CERTIFICATE", []byte("junk"))}, want: "block 0 is not an X.509 certificate"},
		{name: "headers", items: []string{withHeader}, want: "carries PEM headers"},
		{name: "empty", items: []string{" \n"}, want: "layer certificate 0: empty"},
		{name: "duplicate", items: []string{a, b, a}, want: "layer certificate 2: block 0 repeats certificate 0"},
		{name: "duplicate in one item", items: []string{a + a}, want: "block 1 repeats certificate 0"},
		{name: "too many", items: many, want: "more than 16 layer certificates"},
		{name: "too large", items: []string{block("CERTIFICATE", make([]byte, layercert.MaxCertificateBytes+1))}, want: "larger than"},
		{name: "item too large", items: []string{strings.Repeat(" ", 1<<20) + a}, want: "layer certificate 0: larger than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := layercert.Normalize(tc.items)
			assert.Nil(t, got)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), strings.Split(key, "\n")[1], "key material is never echoed")
		})
	}
}

func TestDigestsRejectsNonCanonical(t *testing.T) {
	a, _, _ := newCert(t, "a")
	_, err := layercert.Digests([]string{a + a})
	assert.ErrorContains(t, err, "layer certificate 0 is not one PEM certificate")
	_, err = layercert.Digests([]string{"junk"})
	assert.Error(t, err)
}

func TestWord(t *testing.T) {
	_, d, _ := newCert(t, "a")
	w := layercert.Word(d)
	assert.Equal(t, "layer-certificate:"+d, w)
	got, ok := layercert.DigestFromWord(w)
	assert.True(t, ok)
	assert.Equal(t, d, got)

	for _, bad := range []string{
		"layer-certificate:" + strings.ToUpper(d),
		"layer-certificate:" + d[:62],
		"layer-certificate:" + d + "00",
		"layer-certificate:" + strings.Repeat("g", 64),
		"machine-id:" + d,
	} {
		_, ok := layercert.DigestFromWord(bad)
		assert.False(t, ok, bad)
	}
}
