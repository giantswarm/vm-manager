package attest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/vm-manager/internal/layercert"
)

// PCRSystemIdentity is PCR 15, systemd's system-identity PCR: the initrd
// extends it with one layercert.Word per installation layer certificate
// before it seals the .dm-verity keyring, next to systemd's own machine-id
// and file-system measurements. Quoted at the ready stage; its value is not
// golden, the verifier replays the userspace log against it.
const PCRSystemIdentity = 15

// logRecord is the part of a record of systemd's userspace measurement log
// (/run/log/systemd/tpm2-measure.log, a JSON-SEQ stream of TCG CEL-JSON
// records) the verifier reads:
//
//	{"pcr":15,"digests":[{"hashAlg":"sha256","digest":"<hex>"},...],
//	 "content_type":"systemd","content":{"string":"<word>",...}}
//
// Records without "pcr" (NvPCR extensions) are skipped.
type logRecord struct {
	PCR     *int `json:"pcr"`
	Digests []struct {
		HashAlg string `json:"hashAlg"`
		Digest  string `json:"digest"`
	} `json:"digests"`
	ContentType string          `json:"content_type"`
	Content     json.RawMessage `json:"content"`
}

// recordSeparator starts every JSON-SEQ record (RFC 7464).
const recordSeparator = 0x1e

// measuredLayerCertificates replays the PCR 15 records of the userspace log
// in the sha256 bank from zero and returns the layer certificate digests the
// records up to the quoted value carry, in order. Every record extends the
// replay; only layer certificate words are collected, and their digest must
// be the sha256 of the word. The replay stops at the first record that
// reproduces the quoted value (records appended after the quote was taken
// are not covered by it); none reproducing it is an error.
func measuredLayerCertificates(log, quoted []byte) ([]string, error) {
	pcr := make([]byte, sha256.Size)
	if bytes.Equal(pcr, quoted) {
		return nil, nil
	}
	var digests []string
	n := 0
	for i, raw := range bytes.Split(log, []byte{recordSeparator}) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var rec logRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("userspace log record %d: %v", i, err)
		}
		if rec.PCR == nil || *rec.PCR != PCRSystemIdentity {
			continue
		}
		n++
		digest, err := rec.sha256()
		if err != nil {
			return nil, fmt.Errorf("userspace log record %d: %v", i, err)
		}
		if word, ok := rec.word(); ok && strings.HasPrefix(word, layercert.WordPrefix) {
			cert, ok := layercert.DigestFromWord(word)
			if !ok {
				return nil, fmt.Errorf("userspace log record %d: %q is not %s<lowercase hex sha256>", i, word, layercert.WordPrefix)
			}
			if sum := sha256.Sum256([]byte(word)); !bytes.Equal(sum[:], digest) {
				return nil, fmt.Errorf("userspace log record %d: its sha256 digest is not the digest of %q", i, word)
			}
			digests = append(digests, cert)
		}
		h := sha256.New()
		h.Write(pcr)
		h.Write(digest)
		pcr = h.Sum(nil)
		if bytes.Equal(pcr, quoted) {
			return digests, nil
		}
	}
	return nil, fmt.Errorf("the %d pcr 15 records of the userspace log replay to %x, the quote has %x", n, pcr, quoted)
}

// sha256 is the record's digest in the sha256 bank.
func (r logRecord) sha256() ([]byte, error) {
	for _, d := range r.Digests {
		if d.HashAlg != Bank {
			continue
		}
		b, err := hex.DecodeString(d.Digest)
		if err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("%s digest %q is not %d bytes of hex", Bank, d.Digest, sha256.Size)
		}
		return b, nil
	}
	return nil, fmt.Errorf("no %s digest", Bank)
}

// word is the string systemd measured, for records systemd wrote.
func (r logRecord) word() (string, bool) {
	if r.ContentType != "systemd" {
		return "", false
	}
	var c struct {
		String *string `json:"string"`
	}
	if json.Unmarshal(r.Content, &c) != nil || c.String == nil {
		return "", false
	}
	return *c.String, true
}

// compareLayerCertificates requires the measured digests to be exactly the
// served ones in the served order; the error names what is missing, extra
// or out of order.
func compareLayerCertificates(served, measured []string) error {
	if slices.Equal(served, measured) {
		return nil
	}
	count := make(map[string]int)
	for _, d := range served {
		count[d]++
	}
	for _, d := range measured {
		count[d]--
	}
	var missing, extra []string
	for _, d := range served {
		if count[d] > 0 {
			missing = append(missing, d)
			count[d]--
		}
	}
	for _, d := range measured {
		if count[d] < 0 {
			extra = append(extra, d)
			count[d]++
		}
	}
	var problems []string
	if len(missing) > 0 {
		problems = append(problems, "missing "+strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		problems = append(problems, "not served "+strings.Join(extra, ", "))
	}
	if len(problems) == 0 {
		problems = append(problems, fmt.Sprintf("measured in the order %s, served %s", digestList(measured), digestList(served)))
	}
	return fmt.Errorf("layer certificates measured into pcr 15 differ from those served: %s", strings.Join(problems, "; "))
}

func digestList(digests []string) string {
	if len(digests) == 0 {
		return "(none)"
	}
	return strings.Join(digests, ", ")
}
