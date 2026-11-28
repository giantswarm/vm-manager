// Package layercert handles an installation's layer certificates: the X.509
// certificates a caller hands create_vm as layer_certificates. The IMDS
// serves them to the guest as /layer-certificates; the initrd enrolls them
// into the kernel's .dm-verity keyring next to the image's own layer
// certificate and, before it seals the keyring, extends PCR 15 once per
// certificate in the order served with Word. The attestation verifier
// replays those measurements from the ready quote and requires exactly the
// certificates served (internal/attest).
package layercert

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
)

// Limits on the certificates of one VM.
const (
	// MaxCertificates bounds the list; a rotation overlaps old and new
	// certificates, so a few per installation is the expected size.
	MaxCertificates = 16
	// MaxCertificateBytes bounds the DER of one certificate.
	MaxCertificateBytes = 16 << 10
	// maxItemBytes bounds one PEM string before it is parsed.
	maxItemBytes = 2 * MaxCertificates * MaxCertificateBytes
)

// WordPrefix starts the word the guest extends PCR 15 with for each
// certificate: `systemd-pcrextend --pcr=15 layer-certificate:<digest>`.
const WordPrefix = "layer-certificate:"

// pemType is the only PEM block type accepted.
const pemType = "CERTIFICATE"

// Normalize validates the certificates a caller passed and returns them in
// canonical PEM, one certificate per element, in the caller's order. An
// element may hold several CERTIFICATE blocks and whitespace between them;
// anything else (another block type such as a private key, text outside the
// blocks, PEM headers, a block that is not an X.509 certificate, a
// certificate given twice) is an error that names the element and block but
// never echoes their content. Nil or empty input returns nil.
func Normalize(items []string) ([]string, error) {
	var out []string
	seen := make(map[string]int)
	for i, item := range items {
		if len(item) > maxItemBytes {
			return nil, fmt.Errorf("layer certificate %d: larger than %d bytes", i, maxItemBytes)
		}
		rest := bytes.TrimSpace([]byte(item))
		if len(rest) == 0 {
			return nil, fmt.Errorf("layer certificate %d: empty", i)
		}
		for n := 0; len(rest) > 0; n++ {
			if !bytes.HasPrefix(rest, []byte("-----BEGIN ")) {
				return nil, fmt.Errorf("layer certificate %d: text outside a PEM block after block %d", i, n)
			}
			block, r := pem.Decode(rest)
			if block == nil {
				return nil, fmt.Errorf("layer certificate %d: block %d is not valid PEM", i, n)
			}
			rest = bytes.TrimSpace(r)
			if block.Type != pemType {
				return nil, fmt.Errorf("layer certificate %d: block %d is a %q block, want %q", i, n, block.Type, pemType)
			}
			if len(block.Headers) > 0 {
				return nil, fmt.Errorf("layer certificate %d: block %d carries PEM headers", i, n)
			}
			if len(block.Bytes) > MaxCertificateBytes {
				return nil, fmt.Errorf("layer certificate %d: block %d is larger than %d bytes", i, n, MaxCertificateBytes)
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return nil, fmt.Errorf("layer certificate %d: block %d is not an X.509 certificate: %v", i, n, err)
			}
			digest := digest(block.Bytes)
			if first, dup := seen[digest]; dup {
				return nil, fmt.Errorf("layer certificate %d: block %d repeats certificate %d (sha256 %s)", i, n, first, digest)
			}
			seen[digest] = len(out)
			out = append(out, string(pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: block.Bytes})))
			if len(out) > MaxCertificates {
				return nil, fmt.Errorf("more than %d layer certificates", MaxCertificates)
			}
		}
	}
	return out, nil
}

// Digests returns the lowercase hex sha256 of each certificate's DER, in
// order: what the guest measures (Word) and the verifier compares. The
// input is Normalize's output; anything else is an error.
func Digests(pems []string) ([]string, error) {
	out := make([]string, 0, len(pems))
	for i, p := range pems {
		block, rest := pem.Decode([]byte(p))
		if block == nil || block.Type != pemType || len(bytes.TrimSpace(rest)) > 0 {
			return nil, fmt.Errorf("layer certificate %d is not one PEM certificate", i)
		}
		out = append(out, digest(block.Bytes))
	}
	return out, nil
}

// Word is what the guest extends PCR 15 with for the certificate whose DER
// has the given digest.
func Word(digest string) string {
	return WordPrefix + digest
}

// DigestFromWord returns the digest a measured word names and whether the
// word is a layer certificate word with a well-formed digest (64 lowercase
// hex characters).
func DigestFromWord(word string) (string, bool) {
	d, ok := strings.CutPrefix(word, WordPrefix)
	if !ok || len(d) != 2*sha256.Size || strings.ToLower(d) != d {
		return "", false
	}
	if _, err := hex.DecodeString(d); err != nil {
		return "", false
	}
	return d, true
}

func digest(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}
