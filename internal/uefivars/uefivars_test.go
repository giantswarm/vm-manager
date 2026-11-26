package uefivars

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func emptyStore(t *testing.T) []byte {
	t.Helper()
	return Blank()
}

func testCert(t *testing.T, cn string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(0, 0).Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	c, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return c
}

func TestGUID(t *testing.T) {
	g := MustParseGUID("d719b2cb-3d3a-4596-a3bc-dad00e67656f")
	// EFI byte order: Data1..3 little-endian.
	assert.Equal(t, []byte{0xcb, 0xb2, 0x19, 0xd7, 0x3a, 0x3d, 0x96, 0x45, 0xa3, 0xbc, 0xda, 0xd0, 0x0e, 0x67, 0x65, 0x6f}, g[:])
	assert.Equal(t, "d719b2cb-3d3a-4596-a3bc-dad00e67656f", g.String())
	_, err := ParseGUID("d719b2cb3d3a-4596-a3bc-dad00e67656f-")
	assert.Error(t, err)
	_, err = ParseGUID("x719b2cb-3d3a-4596-a3bc-dad00e67656f")
	assert.Error(t, err)
}

func TestParseRejects(t *testing.T) {
	for name, mutate := range map[string]func([]byte) []byte{
		"short":            func(d []byte) []byte { return d[:0x50] },
		"no _FVH":          func(d []byte) []byte { d[40] = 'X'; return d },
		"other fs":         func(d []byte) []byte { d[16] ^= 1; return d },
		"non-auth store":   func(d []byte) []byte { d[0x48] ^= 1; return d },
		"unhealthy":        func(d []byte) []byte { d[0x5d] = 0xff; return d },
		"store past file":  func(d []byte) []byte { binary.LittleEndian.PutUint32(d[0x58:0x5c], 0x100000); return d },
		"header too short": func(d []byte) []byte { binary.LittleEndian.PutUint16(d[48:50], 0x10); return d },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(mutate(emptyStore(t)))
			assert.Error(t, err)
		})
	}
}

func TestSetAndReplace(t *testing.T) {
	data := emptyStore(t)
	s, err := Parse(data)
	require.NoError(t, err)
	assert.Empty(t, s.Variables())
	assert.Equal(t, 0x64, s.free, "first variable right after the store header")

	a, b := testCert(t, "a"), testCert(t, "b")
	require.NoError(t, s.Set("db", ImageSecurityDatabase, AttrDB, SignatureList(a)))
	first := s.free
	require.NoError(t, s.Set("db", ImageSecurityDatabase, AttrDB, SignatureList(a, b)))

	// Re-parse what is in the bytes: one live db (the second), the first
	// marked deleted, both still present as the firmware would leave them.
	s2, err := Parse(s.Bytes())
	require.NoError(t, err)
	vars := s2.Variables()
	require.Len(t, vars, 1)
	assert.Equal(t, "db", vars[0].Name)
	assert.Equal(t, ImageSecurityDatabase, vars[0].GUID)
	assert.Equal(t, uint32(0x27), vars[0].Attributes)
	assert.Equal(t, SignatureList(a, b), vars[0].Data)
	assert.Equal(t, first, vars[0].offset, "the replacement is appended after the deleted one")
	assert.Equal(t, byte(varAdded&varDeleted), data[0x64+2], "the first db is marked deleted")
	assert.Zero(t, s2.free%headerAlignment, "headers stay 4-byte aligned")
	assert.Equal(t, byte(0xff), data[s2.free], "erased flash after the last variable")

	got, ok := s2.Get("db", ImageSecurityDatabase)
	require.True(t, ok)
	assert.Equal(t, vars[0].Data, got.Data)
	_, ok = s2.Get("db", Owner)
	assert.False(t, ok, "another vendor GUID")
}

func TestSetFull(t *testing.T) {
	s, err := Parse(emptyStore(t))
	require.NoError(t, err)
	err = s.Set("big", Owner, AttrDB, make([]byte, 0x40000))
	assert.ErrorContains(t, err, "do not fit")
}

// TestSignatureList pins the EFI_SIGNATURE_LIST encoding the firmware
// measures into PCR 7 and the kernel parses into its platform keyring.
func TestSignatureList(t *testing.T) {
	a, b := testCert(t, "a"), testCert(t, "bb")
	l := SignatureList(a, b)
	off := 0
	for _, c := range []*x509.Certificate{a, b} {
		var typ GUID
		copy(typ[:], l[off:off+16])
		assert.Equal(t, certX509, typ)
		listSize := int(binary.LittleEndian.Uint32(l[off+16:]))
		assert.Zero(t, binary.LittleEndian.Uint32(l[off+20:]), "SignatureHeaderSize")
		sigSize := int(binary.LittleEndian.Uint32(l[off+24:]))
		assert.Equal(t, 16+len(c.Raw), sigSize)
		assert.Equal(t, 28+sigSize, listSize)
		var owner GUID
		copy(owner[:], l[off+28:off+44])
		assert.Equal(t, Owner, owner)
		assert.Equal(t, c.Raw, l[off+44:off+listSize])
		off += listSize
	}
	assert.Equal(t, len(l), off)
}

func TestEnrollDB(t *testing.T) {
	dir := t.TempDir()
	vars := filepath.Join(dir, "vars.fd")
	require.NoError(t, os.WriteFile(vars, emptyStore(t), 0o600))
	c := testCert(t, "image")

	changes, err := EnrollDB(vars, c)
	require.NoError(t, err)
	assert.Equal(t, []string{"db written"}, changes)
	st, err := os.Stat(vars)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(), "mode kept")
	assert.Equal(t, int64(0x84000), st.Size(), "size kept")
	data, err := os.ReadFile(vars) // #nosec G304 -- the test's temp file
	require.NoError(t, err)
	s, err := Parse(data)
	require.NoError(t, err)
	v, ok := s.Get("db", ImageSecurityDatabase)
	require.True(t, ok)
	assert.Equal(t, SignatureList(c), v.Data)

	// Already in that state: nothing changes, the file is not rewritten.
	before, err := os.Stat(vars)
	require.NoError(t, err)
	changes, err = EnrollDB(vars, c)
	require.NoError(t, err)
	assert.Empty(t, changes)
	after, err := os.Stat(vars)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "an unchanged store is left in place")

	// Deterministic: the same certificate gives the same store, which keeps
	// PCR 7 the same for every VM of an image.
	other := filepath.Join(dir, "other.fd")
	require.NoError(t, os.WriteFile(other, emptyStore(t), 0o600))
	_, err = EnrollDB(other, c)
	require.NoError(t, err)
	otherData, err := os.ReadFile(other) // #nosec G304 -- the test's temp file
	require.NoError(t, err)
	assert.Equal(t, data, otherData)

	_, err = EnrollDB(vars)
	assert.Error(t, err, "no certificate")
	_, err = EnrollDB(filepath.Join(dir, "missing.fd"), c)
	assert.Error(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "junk.fd"), []byte("junk"), 0o600))
	_, err = EnrollDB(filepath.Join(dir, "junk.fd"), c)
	assert.Error(t, err)
}

// TestEnrollDBUndoesTheGuest: what a guest in setup mode may have written
// (its own certificate in db, a PK, KEK or dbx) is gone after the next
// enrollment, and db is the image's again.
func TestEnrollDBUndoesTheGuest(t *testing.T) {
	vars := filepath.Join(t.TempDir(), "vars.fd")
	data := emptyStore(t)
	img, guest := testCert(t, "image"), testCert(t, "guest")
	s, err := Parse(data)
	require.NoError(t, err)
	require.NoError(t, s.Set("db", ImageSecurityDatabase, AttrDB, SignatureList(img, guest)))
	require.NoError(t, s.Set("PK", GlobalVariable, AttrDB, SignatureList(guest)))
	require.NoError(t, s.Set("KEK", GlobalVariable, AttrDB, SignatureList(guest)))
	require.NoError(t, s.Set("dbx", ImageSecurityDatabase, AttrDB, SignatureList(img)))
	require.NoError(t, s.Set("BootOrder", GlobalVariable, 0x7, []byte{1, 0}))
	require.NoError(t, os.WriteFile(vars, s.Bytes(), 0o600))

	changes, err := EnrollDB(vars, img)
	require.NoError(t, err)
	assert.Equal(t, []string{"PK removed", "KEK removed", "dbx removed", "db replaced"}, changes)
	got, err := os.ReadFile(vars) // #nosec G304 -- the test's temp file
	require.NoError(t, err)
	s, err = Parse(got)
	require.NoError(t, err)
	names := []string{}
	for _, v := range s.Variables() {
		names = append(names, v.Name)
	}
	assert.ElementsMatch(t, []string{"BootOrder", "db"}, names, "other variables stay")
	db, _ := s.Get("db", ImageSecurityDatabase)
	assert.Equal(t, SignatureList(img), db.Data)
}

// TestSetCompacts: a store whose free space deleted variables used up is
// compacted the way the firmware reclaims it, live records unchanged.
func TestSetCompacts(t *testing.T) {
	s, err := Parse(emptyStore(t))
	require.NoError(t, err)
	require.NoError(t, s.Set("BootOrder", GlobalVariable, 0x7, []byte{1, 0}))
	boot, _ := s.Get("BootOrder", GlobalVariable)
	record := append([]byte(nil), s.data[boot.offset:boot.offset+varHeaderSize+len(encodeName("BootOrder"))+2]...)
	big := bytes.Repeat([]byte{0xab}, 0x10000)
	for range 10 {
		require.NoError(t, s.Set("db", ImageSecurityDatabase, AttrDB, big))
	}
	s2, err := Parse(s.Bytes())
	require.NoError(t, err)
	names := []string{}
	for _, v := range s2.Variables() {
		names = append(names, v.Name)
	}
	assert.Equal(t, []string{"BootOrder", "db"}, names)
	boot, _ = s2.Get("BootOrder", GlobalVariable)
	assert.Equal(t, record, s2.data[boot.offset:boot.offset+len(record)], "the record is moved, not rewritten")
	db, _ := s2.Get("db", ImageSecurityDatabase)
	assert.Equal(t, big, db.Data)
	assert.Less(t, s2.free, s2.start+0x30000, "the deleted copies are gone")
}

// TestHostTemplates enrolls into the firmware variable templates this host
// has, the files vm-manager copies for each VM.
func TestHostTemplates(t *testing.T) {
	c := testCert(t, "image")
	found := 0
	for _, p := range []string{
		"/usr/share/edk2/x64/OVMF_VARS.4m.fd",
		"/usr/share/OVMF/OVMF_VARS_4M.fd",
		"/usr/share/OVMF/OVMF_VARS.fd",
		"/usr/share/edk2/ovmf/OVMF_VARS.fd",
	} {
		data, err := os.ReadFile(p) // #nosec G304 -- fixed paths
		if err != nil {
			continue
		}
		found++
		t.Run(filepath.Base(p), func(t *testing.T) {
			s, err := Parse(data)
			require.NoError(t, err)
			require.NoError(t, s.Set("db", ImageSecurityDatabase, AttrDB, SignatureList(c)))
			s2, err := Parse(s.Bytes())
			require.NoError(t, err)
			_, ok := s2.Get("db", ImageSecurityDatabase)
			assert.True(t, ok)
		})
	}
	if found == 0 {
		t.Skip("no OVMF variable template on this host")
	}
}

func TestParseCertificates(t *testing.T) {
	a, b := testCert(t, "a"), testCert(t, "b")
	var pemData []byte
	pemData = append(pemData, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")})...)
	pemData = append(pemData, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.Raw})...)
	pemData = append(pemData, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.Raw})...)
	certs, err := ParseCertificates(pemData)
	require.NoError(t, err)
	require.Len(t, certs, 2)
	assert.Equal(t, "a", certs[0].Subject.CommonName)
	assert.Equal(t, "b", certs[1].Subject.CommonName)

	certs, err = ParseCertificates(a.Raw)
	require.NoError(t, err)
	require.Len(t, certs, 1)

	_, err = ParseCertificates([]byte("not a certificate"))
	assert.Error(t, err)
	_, err = ParseCertificates(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}))
	assert.Error(t, err)
}
