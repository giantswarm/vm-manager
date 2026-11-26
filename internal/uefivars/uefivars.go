// Package uefivars edits the variable store of an OVMF (EDK2) firmware
// volume, the file QEMU maps as the writable pflash next to the read-only
// firmware code. vm-manager uses it for one thing: enrolling a guest image's
// signing certificate into the VM's UEFI signature database (db), so that the
// guest kernel loads it onto its platform keyring and verifies the image's
// dm-verity root hash signatures and its IPE policy against it
// (images/README.md, IPE). Secure Boot stays off: no PK or KEK is written, the
// firmware remains in setup mode and boots what it booted before.
//
// Setup mode authenticates no write to the Secure Boot variables, so root in
// the guest can change db at runtime, and a reset of the same QEMU process
// would boot with it. EnrollDB is therefore not a one-time enrollment but the
// state vm-manager writes before every start of a VM's QEMU (Spec.UEFIDB in
// internal/runtime/qemu), and a guest's reboot is a new QEMU process: db is
// the image's certificate and PK, KEK, dbx, dbt and dbr are absent, whatever
// the previous boot wrote.
//
// The store is the authenticated-variable format of
// MdeModulePkg/Include/Guid/VariableFormat.h: an EFI_FIRMWARE_VOLUME_HEADER
// whose file system GUID is EFI_SYSTEM_NV_DATA_FV_GUID, a
// VARIABLE_STORE_HEADER with gEfiAuthenticatedVariableGuid, then
// AUTHENTICATED_VARIABLE_HEADERs (start id 0x55AA), each followed by its
// UTF-16 name and data and aligned to four bytes; erased flash is 0xFF.
// Variables are only ever appended or marked deleted, the way the firmware
// itself updates the store, so the result is what the firmware would have
// written for the same SetVariable() calls in setup mode.
package uefivars

import (
	"bytes"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf16"
)

// GUID is an EFI_GUID in its in-memory (mixed-endian) byte order.
type GUID [16]byte

// MustParseGUID parses the textual form "aabbccdd-eeff-0011-2233-445566778899".
func MustParseGUID(s string) GUID {
	g, err := ParseGUID(s)
	if err != nil {
		panic(err)
	}
	return g
}

// ParseGUID parses the textual form of a GUID into EFI byte order: the first
// three groups little-endian, the last two as written.
func ParseGUID(s string) (GUID, error) {
	var g GUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return g, fmt.Errorf("guid %q: want 8-4-4-4-12 hex digits", s)
	}
	b, err := hex.DecodeString(s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36])
	if err != nil {
		return g, fmt.Errorf("guid %q: %w", s, err)
	}
	// Data1 (4 bytes), Data2 and Data3 (2 bytes each) are little-endian.
	g[0], g[1], g[2], g[3] = b[3], b[2], b[1], b[0]
	g[4], g[5] = b[5], b[4]
	g[6], g[7] = b[7], b[6]
	copy(g[8:], b[8:])
	return g, nil
}

// String formats the GUID the way ParseGUID reads it.
func (g GUID) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x",
		binary.LittleEndian.Uint32(g[0:4]), binary.LittleEndian.Uint16(g[4:6]),
		binary.LittleEndian.Uint16(g[6:8]), g[8:10], g[10:16])
}

var (
	// nvDataFV is EFI_SYSTEM_NV_DATA_FV_GUID, the file system of the
	// variable firmware volume.
	nvDataFV = MustParseGUID("fff12b8d-7696-4c8b-a985-2747075b4f50")
	// authVariableStore is gEfiAuthenticatedVariableGuid, the signature of
	// a store whose variables carry AUTHENTICATED_VARIABLE_HEADERs.
	authVariableStore = MustParseGUID("aaf32c78-947b-439a-a180-2e144ec37792")
	// ImageSecurityDatabase is EFI_IMAGE_SECURITY_DATABASE_GUID, the vendor
	// GUID of db, dbx and dbt.
	ImageSecurityDatabase = MustParseGUID("d719b2cb-3d3a-4596-a3bc-dad00e67656f")
	// GlobalVariable is EFI_GLOBAL_VARIABLE, the vendor GUID of PK and KEK.
	GlobalVariable = MustParseGUID("8be4df61-93ca-11d2-aa0d-00e098032b8c")
	// certX509 is EFI_CERT_X509_GUID, the signature type of a DER X.509
	// certificate in an EFI_SIGNATURE_LIST.
	certX509 = MustParseGUID("a5c059a1-94e4-4aa7-87b5-ab155c2bf072")
	// Owner is the SignatureOwner vm-manager writes into every
	// EFI_SIGNATURE_DATA. It is part of what the firmware measures into
	// PCR 7, so it never changes.
	Owner = MustParseGUID("616b7bd7-485c-4087-a62c-e7e4a476b592")
)

const (
	fvSignature       = "_FVH"
	fvHeaderMinLength = 0x48

	storeHeaderSize = 28
	storeFormatted  = 0x5a
	storeHealthy    = 0xfe

	varStartID      = 0x55aa
	varHeaderSize   = 60
	varAdded        = 0x3f
	varInDeletedTr  = 0xfe
	varDeleted      = 0xfd
	headerAlignment = 4

	// AttrDB are the attributes of db: non-volatile, boot service and
	// runtime access, time-based authenticated writes.
	AttrDB = 0x00000001 | 0x00000002 | 0x00000004 | 0x00000020

	signatureListHeaderSize = 28
	signatureOwnerSize      = 16
)

// Blank returns a stand-in for an OVMF variable template, for tests: a
// firmware volume of the edk2 4 MiB size with an empty, healthy
// authenticated variable store at the offsets those templates use. Unlike a
// real template it has no volume attributes, block map, checksum or
// fault-tolerant-write areas, none of which Parse reads.
func Blank() []byte {
	const fvLength, storeSize = 0x84000, 0x3ffb8
	data := bytes.Repeat([]byte{0xff}, fvLength)
	clear(data[0:16])
	copy(data[16:32], nvDataFV[:])
	binary.LittleEndian.PutUint64(data[32:40], fvLength)
	copy(data[40:44], fvSignature)
	clear(data[44:fvHeaderMinLength])
	binary.LittleEndian.PutUint16(data[48:50], fvHeaderMinLength)
	copy(data[fvHeaderMinLength:], authVariableStore[:])
	binary.LittleEndian.PutUint32(data[fvHeaderMinLength+16:], storeSize)
	data[fvHeaderMinLength+20], data[fvHeaderMinLength+21] = storeFormatted, storeHealthy
	clear(data[fvHeaderMinLength+22 : fvHeaderMinLength+storeHeaderSize])
	return data
}

// Variable is a live variable of a store.
type Variable struct {
	Name       string
	GUID       GUID
	Attributes uint32
	Data       []byte
	// offset is where the header starts in the store's file.
	offset int
	state  byte
}

// Store is a parsed variable firmware volume.
type Store struct {
	data []byte
	// start and end bound the variable area: the first header and the end
	// of the VARIABLE_STORE_HEADER's Size.
	start, end int
	// free is the offset after the last variable header.
	free int
	vars []Variable
}

// Parse reads a variable firmware volume. The slice is kept and modified by
// the Store's methods.
func Parse(data []byte) (*Store, error) {
	if len(data) < fvHeaderMinLength+storeHeaderSize {
		return nil, fmt.Errorf("variable store: %d bytes is too short for a firmware volume", len(data))
	}
	if string(data[40:44]) != fvSignature {
		return nil, errors.New("variable store: no _FVH firmware volume signature at offset 40")
	}
	var fs GUID
	copy(fs[:], data[16:32])
	if fs != nvDataFV {
		return nil, fmt.Errorf("variable store: firmware volume file system %s is not EFI_SYSTEM_NV_DATA_FV_GUID", fs)
	}
	hdrLen := int(binary.LittleEndian.Uint16(data[48:50]))
	if hdrLen < fvHeaderMinLength || hdrLen+storeHeaderSize > len(data) {
		return nil, fmt.Errorf("variable store: firmware volume header length %#x out of range", hdrLen)
	}
	var sig GUID
	copy(sig[:], data[hdrLen:hdrLen+16])
	if sig != authVariableStore {
		return nil, fmt.Errorf("variable store: signature %s is not the authenticated variable store", sig)
	}
	size := int(binary.LittleEndian.Uint32(data[hdrLen+16 : hdrLen+20]))
	format, state := data[hdrLen+20], data[hdrLen+21]
	if format != storeFormatted || state != storeHealthy {
		return nil, fmt.Errorf("variable store: format %#x state %#x, want formatted (%#x) and healthy (%#x)", format, state, storeFormatted, storeHealthy)
	}
	s := &Store{data: data, start: align(hdrLen + storeHeaderSize), end: hdrLen + size}
	if s.end > len(data) || s.start > s.end {
		return nil, fmt.Errorf("variable store: size %#x runs past the %d-byte file", size, len(data))
	}
	p := s.start
	for p+varHeaderSize <= s.end && binary.LittleEndian.Uint16(data[p:p+2]) == varStartID {
		v, next, err := s.readVariable(p)
		if err != nil {
			return nil, err
		}
		if v.state == varAdded || v.state == varAdded&varInDeletedTr {
			s.vars = append(s.vars, v)
		}
		p = next
	}
	s.free = p
	return s, nil
}

func (s *Store) readVariable(p int) (Variable, int, error) {
	d := s.data
	v := Variable{offset: p, state: d[p+2], Attributes: binary.LittleEndian.Uint32(d[p+4 : p+8])}
	nameSize := int(binary.LittleEndian.Uint32(d[p+36 : p+40]))
	dataSize := int(binary.LittleEndian.Uint32(d[p+40 : p+44]))
	copy(v.GUID[:], d[p+44:p+60])
	nameAt := p + varHeaderSize
	dataAt := nameAt + nameSize
	next := align(dataAt + dataSize)
	if nameSize < 0 || dataSize < 0 || dataAt+dataSize > s.end || nameSize%2 != 0 {
		return v, 0, fmt.Errorf("variable store: variable at %#x has name size %d and data size %d past the store end %#x", p, nameSize, dataSize, s.end)
	}
	v.Name = decodeName(d[nameAt:dataAt])
	v.Data = d[dataAt : dataAt+dataSize]
	return v, next, nil
}

// Variables returns the live variables.
func (s *Store) Variables() []Variable { return append([]Variable(nil), s.vars...) }

// Get returns the live variable name/guid.
func (s *Store) Get(name string, guid GUID) (Variable, bool) {
	for _, v := range s.vars {
		if v.Name == name && v.GUID == guid {
			return v, true
		}
	}
	return Variable{}, false
}

// Delete marks every live record of name/guid deleted (a store should hold
// one, a damaged or crafted one may hold more) and reports whether there
// was any.
func (s *Store) Delete(name string, guid GUID) bool {
	found := false
	kept := s.vars[:0]
	for _, v := range s.vars {
		if v.Name == name && v.GUID == guid {
			s.data[v.offset+2] &= varDeleted
			found = true
			continue
		}
		kept = append(kept, v)
	}
	s.vars = kept
	return found
}

// records are the live records of name/guid, in store order.
func (s *Store) records(name string, guid GUID) []Variable {
	var out []Variable
	for _, v := range s.vars {
		if v.Name == name && v.GUID == guid {
			out = append(out, v)
		}
	}
	return out
}

// Set writes the variable, marking a live one of the same name and GUID
// deleted. When the appended copy does not fit, the store is compacted
// first, as the firmware reclaims it: deleted variables are dropped and the
// live ones moved together, byte for byte. The timestamp and monotonic count
// of the authenticated header are zero and no public key index is set: the
// store is written in setup mode, where the firmware applies no
// authentication, and none of these fields is measured.
func (s *Store) Set(name string, guid GUID, attributes uint32, data []byte) error {
	nameBytes := encodeName(name)
	need := varHeaderSize + len(nameBytes) + len(data)
	s.Delete(name, guid)
	if s.free+need > s.end {
		s.compact()
	}
	if s.free+need > s.end {
		return fmt.Errorf("variable store: %d bytes for %s do not fit, %d free", need, name, s.end-s.free)
	}
	p := s.free
	h := s.data[p : p+varHeaderSize]
	for i := range h {
		h[i] = 0
	}
	binary.LittleEndian.PutUint16(h[0:2], varStartID)
	h[2] = varAdded
	binary.LittleEndian.PutUint32(h[4:8], attributes)
	// h[8:16] MonotonicCount, h[16:32] EFI_TIME, h[32:36] PubKeyIndex: zero.
	// Both fit the store, which a 32-bit Size bounds.
	binary.LittleEndian.PutUint32(h[36:40], uint32(len(nameBytes))) // #nosec G115 -- bounded by the store size checked above
	binary.LittleEndian.PutUint32(h[40:44], uint32(len(data)))      // #nosec G115 -- bounded by the store size checked above
	copy(h[44:60], guid[:])
	copy(s.data[p+varHeaderSize:], nameBytes)
	copy(s.data[p+varHeaderSize+len(nameBytes):], data)
	v, next, err := s.readVariable(p)
	if err != nil {
		return err
	}
	s.vars = append(s.vars, v)
	s.free = next
	return nil
}

// compact rewrites the variable area with the live variables only, each
// record (header, name, data) copied unchanged and marked added, and erases
// the rest.
func (s *Store) compact() {
	records := make([][]byte, 0, len(s.vars))
	for _, v := range s.vars {
		_, next, _ := s.readVariable(v.offset)
		records = append(records, append([]byte(nil), s.data[v.offset:next]...))
	}
	area := s.data[s.start:s.end]
	for i := range area {
		area[i] = 0xff
	}
	s.vars, s.free = nil, s.start
	for _, r := range records {
		copy(s.data[s.free:], r)
		s.data[s.free+2] = varAdded
		v, next, _ := s.readVariable(s.free)
		s.vars = append(s.vars, v)
		s.free = next
	}
}

// Bytes returns the store's file content.
func (s *Store) Bytes() []byte { return s.data }

// SignatureList encodes the certificates as the content of db: one
// EFI_SIGNATURE_LIST of type EFI_CERT_X509_GUID per certificate (a list's
// entries share one size, certificates differ in length), each with a
// single EFI_SIGNATURE_DATA owned by Owner.
func SignatureList(certs ...*x509.Certificate) []byte {
	var buf bytes.Buffer
	for _, c := range certs {
		sigSize := signatureOwnerSize + len(c.Raw)
		hdr := make([]byte, signatureListHeaderSize)
		copy(hdr[0:16], certX509[:])
		binary.LittleEndian.PutUint32(hdr[16:20], uint32(signatureListHeaderSize+sigSize)) // #nosec G115 -- a parsed X.509 certificate, far below 4 GiB
		// hdr[20:24] SignatureHeaderSize: 0.
		binary.LittleEndian.PutUint32(hdr[24:28], uint32(sigSize)) // #nosec G115 -- as above
		buf.Write(hdr)
		buf.Write(Owner[:])
		buf.Write(c.Raw)
	}
	return buf.Bytes()
}

// removed are the Secure Boot variables EnrollDB keeps out of a store: with
// a PK the firmware leaves setup mode (and boots only what its db signed),
// KEK authorizes db updates, dbx, dbt and dbr revoke or add trust (the guest
// kernel loads dbx onto its blacklist keyring).
var removed = []struct {
	name string
	guid GUID
}{
	{"PK", GlobalVariable}, {"KEK", GlobalVariable},
	{"dbx", ImageSecurityDatabase}, {"dbt", ImageSecurityDatabase}, {"dbr", ImageSecurityDatabase},
}

// EnrollDB makes the variable store file at path hold the certificates as
// db, with AttrDB, and none of PK, KEK, dbx, dbt and dbr. It returns what it
// changed ("db written", "db replaced", "PK removed", ...), nothing for a
// store that already was in that state, which it leaves untouched;
// otherwise the file is replaced (a temporary file renamed over it).
func EnrollDB(path string, certs ...*x509.Certificate) ([]string, error) {
	if len(certs) == 0 {
		return nil, errors.New("enroll db: no certificate")
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the VM's own variable store.
	if err != nil {
		return nil, fmt.Errorf("enroll db: %w", err)
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("enroll db: %s: %w", path, err)
	}
	var changes []string
	for _, r := range removed {
		if s.Delete(r.name, r.guid) {
			changes = append(changes, r.name+" removed")
		}
	}
	// Exactly one plain record with the image's content counts as in place:
	// with two live records, or one in deleted transition, which one the
	// firmware reads is not this parser's to decide, so db is rewritten.
	want := SignatureList(certs...)
	switch dbs := s.records("db", ImageSecurityDatabase); {
	case len(dbs) == 0:
		changes = append(changes, "db written")
	case len(dbs) > 1 || dbs[0].state != varAdded || dbs[0].Attributes != AttrDB || !bytes.Equal(dbs[0].Data, want):
		changes = append(changes, "db replaced")
	}
	if len(changes) == 0 {
		return nil, nil
	}
	if err := s.Set("db", ImageSecurityDatabase, AttrDB, want); err != nil {
		return nil, fmt.Errorf("enroll db: %s: %w", path, err)
	}
	if err := writeFile(path, s.Bytes()); err != nil {
		return nil, fmt.Errorf("enroll db: %w", err)
	}
	return changes, nil
}

// ReadCertificates parses the PEM or DER certificates of a file.
func ReadCertificates(path string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- a certificate of the image directory.
	if err != nil {
		return nil, err
	}
	return ParseCertificates(data)
}

// ParseCertificates parses the CERTIFICATE blocks of PEM data, or data that
// is a single DER certificate.
func ParseCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
	}
	if len(certs) > 0 {
		return certs, nil
	}
	c, err := x509.ParseCertificate(data)
	if err != nil {
		return nil, fmt.Errorf("neither PEM certificates nor a DER certificate: %w", err)
	}
	return []*x509.Certificate{c}, nil
}

func writeFile(path string, data []byte) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(st.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func align(n int) int { return (n + headerAlignment - 1) &^ (headerAlignment - 1) }

func encodeName(name string) []byte {
	u := utf16.Encode([]rune(name))
	b := make([]byte, 2*(len(u)+1))
	for i, r := range u {
		binary.LittleEndian.PutUint16(b[2*i:], r)
	}
	return b
}

func decodeName(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}
