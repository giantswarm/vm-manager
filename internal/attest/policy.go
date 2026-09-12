package attest

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/giantswarm/vm-manager/internal/imds"
)

// Bank is the only PCR bank the policy speaks: sha256, what systemd-measure
// signs and the agent quotes.
const Bank = "sha256"

// Phase paths systemd-pcrphase has extended PCR 11 with when the agent
// quotes; the keys of Policy.PCR11 and what `systemd-measure calculate
// --phase=` was run with by images/scripts/verify.
const (
	PhaseInitrd = "enter-initrd"
	PhaseReady  = "enter-initrd:leave-initrd:sysinit:ready"
)

// PCR indexes and what the policy expects of them.
const (
	// PCRUKI is PCR 11: the UKI and the phase path, fully determined by the
	// image (Policy.PCR11).
	PCRUKI = 11
	// PCRSysext is PCR 13: the stub-loaded system extensions, quoted at the
	// ready stage and compared when the policy has a golden value for it.
	PCRSysext = 13
	// maxPCR is the highest index a PC Client TPM must have.
	maxPCR = 23
)

// firmwarePCRs are PCRs 0-7, the firmware and Secure Boot measurements
// that only the golden values recorded on a known-good boot can vouch for.
var firmwarePCRs = []int{0, 1, 2, 3, 4, 5, 6, 7}

// GoldenIndexes are the PCRs `vm-manager image golden` records: 0-7 and 13.
var GoldenIndexes = append(append([]int(nil), firmwarePCRs...), PCRSysext)

// ErrNoPolicy is returned by a PolicyProvider when the VM's image has no
// usable policy; the verifier rejects the quote with the reason instead of
// failing the request.
var ErrNoPolicy = errors.New("no attestation policy")

// Policy is an image's policy.json as images/scripts/verify writes it and
// `vm-manager image golden` completes it. Fields the verifier does not use
// (uki, roothash, partitions) are ignored. Values are lowercase hex sha256
// digests; golden entries are optional per index.
//
//	{
//	  "image_id": "giantswarm-vm-base",
//	  "image_version": "0.1.0",
//	  "pcr11": {
//	    "enter-initrd": "<hex>",
//	    "enter-initrd:leave-initrd:sysinit:ready": "<hex>"
//	  },
//	  "golden": { "sha256": { "0": "<hex>", ..., "7": "<hex>", "13": "<hex>" } }
//	}
type Policy struct {
	ImageID      string `json:"image_id,omitempty"`
	ImageVersion string `json:"image_version,omitempty"`
	// PCR11 maps a phase path to the expected PCR 11 value.
	PCR11 map[string]string `json:"pcr11"`
	// Golden maps bank -> PCR index -> value recorded on a known-good boot.
	Golden map[string]map[int]string `json:"golden,omitempty"`
}

// ParsePolicy decodes and validates a policy.json.
func ParsePolicy(raw []byte) (Policy, error) {
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return Policy{}, fmt.Errorf("policy: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Validate requires PCR 11 values for both phase paths and well-formed
// golden entries in the sha256 bank only.
func (p Policy) Validate() error {
	for _, phase := range []string{PhaseInitrd, PhaseReady} {
		v, ok := p.PCR11[phase]
		if !ok {
			return fmt.Errorf("policy: pcr11 lacks phase %q", phase)
		}
		if err := checkDigest(v); err != nil {
			return fmt.Errorf("policy: pcr11[%q]: %w", phase, err)
		}
	}
	for bank, values := range p.Golden {
		if bank != Bank {
			return fmt.Errorf("policy: golden bank %q is not supported, only %s", bank, Bank)
		}
		for idx, v := range values {
			if idx < 0 || idx > maxPCR {
				return fmt.Errorf("policy: golden.%s: pcr index %d out of range", bank, idx)
			}
			if err := checkDigest(v); err != nil {
				return fmt.Errorf("policy: golden.%s[%d]: %w", bank, idx, err)
			}
		}
	}
	return nil
}

// golden returns the recorded value of a sha256 PCR, lowercased.
func (p Policy) golden(index int) (string, bool) {
	v, ok := p.Golden[Bank][index]
	return strings.ToLower(v), ok
}

// PhaseFor is the phase path PCR 11 carries at a stage.
func PhaseFor(stage imds.Stage) string {
	if stage == imds.StageReady {
		return PhaseReady
	}
	return PhaseInitrd
}

// RequiredPCRs are the indexes a stage's quote must cover: 0-7 and 11, plus
// 13 at the ready stage.
func RequiredPCRs(stage imds.Stage) []int {
	pcrs := append(append([]int(nil), firmwarePCRs...), PCRUKI)
	if stage == imds.StageReady {
		pcrs = append(pcrs, PCRSysext)
	}
	return pcrs
}

// GoldenFromPCRs picks the golden indexes out of quoted sha256 values, as
// recorded on a Result; indexes the quote did not cover are left out.
func GoldenFromPCRs(pcrs map[int]string) map[int]string {
	out := make(map[int]string, len(GoldenIndexes))
	for _, i := range GoldenIndexes {
		if v, ok := pcrs[i]; ok {
			out[i] = v
		}
	}
	return out
}

func checkDigest(v string) error {
	b, err := hex.DecodeString(v)
	if err != nil {
		return fmt.Errorf("not hex: %w", err)
	}
	if len(b) != 32 {
		return fmt.Errorf("%d bytes, want 32", len(b))
	}
	return nil
}

func indexList(idx []int) string {
	idx = append([]int(nil), idx...)
	sort.Ints(idx)
	parts := make([]string, len(idx))
	for i, v := range idx {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ",")
}
