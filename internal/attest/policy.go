package attest

import (
	"crypto/sha256"
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

// firmwarePCRs are PCRs 0-7, the firmware and Secure Boot measurements;
// every quote covers them and they are recorded per stage.
var firmwarePCRs = []int{0, 1, 2, 3, 4, 5, 6, 7}

// goldenFirmwarePCRs are the firmware PCRs a golden value recorded on one
// known-good boot can vouch for on every VM of the same firmware and image:
// 0 (firmware code), 2 and 3 (option ROMs), 4 (boot loader and UKI), 6
// (nothing but the os-separator), 7 (Secure Boot policy). PCR 1 and 5 are
// quoted and recorded but differ per VM by construction, so they are not
// compared: EDK2 measures the SMBIOS tables into PCR 1, and vm-manager's type
// 11 strings carry per-VM credentials (hostname, machine ID, SSH key, notify
// socket), as does the Boot#### entry with the ESP's partition GUID; PCR 5
// holds the GPT of the installed disk with its per-install partition UUIDs.
// Both are in the firmware event log the agent posts (QuoteRequest.EventLog)
// for a replay-based check per VM (docs/design.md, open points).
var goldenFirmwarePCRs = []int{0, 2, 3, 4, 6, 7}

// GoldenIndexes are the PCRs `vm-manager image golden` records and the
// verifier compares against the policy's golden values: 0, 2-4, 6, 7 and 13.
var GoldenIndexes = append(append([]int(nil), goldenFirmwarePCRs...), PCRSysext)

// ErrNoPolicy is returned by a PolicyProvider when the VM's image has no
// usable policy; the verifier rejects the quote with the reason instead of
// failing the request.
var ErrNoPolicy = errors.New("no attestation policy")

// Sysext components vm-kubernetes.service merges and measures into PCR 13,
// in its order: the Kubernetes sysext first, then the CNI sysext.
const (
	SysextKubernetes = "kubernetes"
	SysextCNI        = "cni"
)

// OSSeparator is the word systemd-pcrosseparator.service measures into PCRs
// 0-7, 9 and 12-14 in the initrd: the first and, without a sysext, the only
// event of PCR 13 in a boot of the image.
const OSSeparator = "os-separator"

// Policy is an image's policy.json as images/scripts/verify and
// verify-kubernetes write it and `vm-manager image golden` completes it.
// Fields the verifier does not use (uki, roothash, partitions) are ignored.
// Values are lowercase hex sha256 digests; sysexts, pcr13 and golden
// entries are optional.
//
//	{
//	  "image_id": "giantswarm-vm-base",
//	  "image_version": "0.1.0",
//	  "pcr11": {
//	    "enter-initrd": "<hex>",
//	    "enter-initrd:leave-initrd:sysinit:ready": "<hex>"
//	  },
//	  "sysexts": {
//	    "kubernetes": { "1.36.4": "<sha256 of kubernetes_1.36.4.raw>" },
//	    "cni": { "cilium-1.20.5": "<sha256 of cni_cilium-1.20.5.raw>" }
//	  },
//	  "pcr15": "layer-certificates",
//	  "golden": { "sha256": { "0": "<hex>", ..., "7": "<hex>", "13": "<hex>" } },
//	  "golden_firmware": { "sha256": "<hex>", "package": "ovmf-generic", "version": "2025.11-3ubuntu7.2" }
//	}
type Policy struct {
	ImageID      string `json:"image_id,omitempty"`
	ImageVersion string `json:"image_version,omitempty"`
	// PCR11 maps a phase path to the expected PCR 11 value.
	PCR11 map[string]string `json:"pcr11"`
	// Sysexts maps a sysext component (SysextKubernetes, SysextCNI) and a
	// published version to the sha256 of <component>_<version>.raw, its
	// SHA256SUMS line. PCR 13 of a VM is computed from the entries of its
	// Kubernetes and CNI versions (SysextPCR), so every combination
	// verifies without an entry per pair. A VM whose versions lack an
	// entry falls back to PCR13, then to Golden.
	Sysexts map[string]map[string]string `json:"sysexts,omitempty"`
	// PCR15 names what the image measures into PCR 15 that the verifier
	// judges: PCR15LayerCertificates, set by images/scripts/verify for
	// every image whose initrd enrolls and measures layer certificates.
	// Empty for an image that predates them.
	PCR15 string `json:"pcr15,omitempty"`
	// PCR13 maps a Kubernetes version to the PCR 13 value of a guest that
	// merged that sysext alone, as images built before Sysexts wrote it;
	// used only for a VM without a CNI version.
	PCR13 map[string]string `json:"pcr13,omitempty"`
	// Golden maps bank -> PCR index -> value recorded on a known-good boot.
	Golden map[string]map[int]string `json:"golden,omitempty"`
	// GoldenFirmware is the firmware build the golden values were recorded
	// under; absent in values recorded before vm-manager wrote it.
	GoldenFirmware *Firmware `json:"golden_firmware,omitempty"`
	// KubernetesVersion selects the PCR13 entry for the VM whose quote is
	// verified. The PolicyProvider sets it; policy.json does not carry it.
	KubernetesVersion string `json:"-"`
	// LayerCertificates are the sha256 digests (layercert.Digests) of the
	// installation layer certificates the IMDS serves the VM, in order; a
	// quoted PCR 15 must measure exactly these, and an image that does not
	// measure layer certificates (PCR15) cannot be served any. The
	// PolicyProvider sets it; policy.json does not carry it.
	LayerCertificates []string `json:"-"`
	// CNIVersion selects the Sysexts entry of the VM's CNI sysext; the
	// PolicyProvider sets it, empty when the VM has none.
	CNIVersion string `json:"-"`
}

// Firmware identifies a build of the firmware code image VMs boot with, as
// get_host reports it: PCR 0 measures the firmware, so golden values
// recorded under one build fail to verify under any other.
type Firmware struct {
	// SHA256 is the hex digest of the code image, the build's identity.
	SHA256 string `json:"sha256"`
	// Package and Version name the package that installed the image, empty
	// when the package manager does not know it.
	Package string `json:"package,omitempty"`
	Version string `json:"version,omitempty"`
}

// SameBuild reports whether o is the same firmware build: the digests match.
func (f Firmware) SameBuild(o Firmware) bool {
	return strings.EqualFold(f.SHA256, o.SHA256)
}

// String names the build for messages: package, version and the digest's
// first 16 hex characters.
func (f Firmware) String() string {
	name := strings.TrimSpace(f.Package + " " + f.Version)
	if name == "" {
		return "sha256 " + short(f.SHA256)
	}
	return name + " (sha256 " + short(f.SHA256) + ")"
}

// SysextMeasurement is the string vm-kubernetes.service measures into PCR
// 13 for each sysext it merged: the line `sha256sum <file>`
// prints in /var/lib/extensions, which is also the file's line in the
// published SHA256SUMS.
func SysextMeasurement(sha256Hex, file string) string {
	return strings.ToLower(sha256Hex) + "  " + file
}

// SysextPCR is the PCR 13 value of a guest that merged sysexts measured as
// measurements, in order: the PCR starts at zero, the initrd extends it
// with OSSeparator and vm-kubernetes.service with each measurement, each as
// PCR := sha256(PCR || sha256(word)). Lowercase hex.
func SysextPCR(measurements ...string) string {
	pcr := make([]byte, sha256.Size)
	for _, word := range append([]string{OSSeparator}, measurements...) {
		digest := sha256.Sum256([]byte(word))
		sum := sha256.Sum256(append(pcr, digest[:]...))
		pcr = sum[:]
	}
	return hex.EncodeToString(pcr)
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

// Validate requires PCR 11 values for both phase paths, well-formed golden
// entries in the sha256 bank only and a firmware digest when one is named.
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
	for version, v := range p.PCR13 {
		if version == "" {
			return errors.New("policy: pcr13 has an entry without a kubernetes version")
		}
		if err := checkDigest(v); err != nil {
			return fmt.Errorf("policy: pcr13[%q]: %w", version, err)
		}
	}
	if p.PCR15 != "" && p.PCR15 != PCR15LayerCertificates {
		return fmt.Errorf("policy: pcr15 %q is not %q", p.PCR15, PCR15LayerCertificates)
	}
	for component, versions := range p.Sysexts {
		for version, v := range versions {
			if component == "" || version == "" {
				return fmt.Errorf("policy: sysexts has an entry without a component or version (%q/%q)", component, version)
			}
			if err := checkDigest(v); err != nil {
				return fmt.Errorf("policy: sysexts.%s[%q]: %w", component, version, err)
			}
		}
	}
	if p.GoldenFirmware != nil {
		if err := checkDigest(p.GoldenFirmware.SHA256); err != nil {
			return fmt.Errorf("policy: golden_firmware.sha256: %w", err)
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

// expected is the value a quoted PCR must have: for PCR 13 the value
// computed from the Sysexts entries of the VM's Kubernetes and CNI
// versions, or the PCR13 entry of a VM without CNI, otherwise the golden
// value. sysexts names the versions the value belongs to ("kubernetes
// 1.36.4, cni cilium-1.20.5"), empty for a golden value.
func (p Policy) expected(index int) (want, sysexts string, ok bool) {
	if index == PCRSysext && p.KubernetesVersion != "" {
		measured := p.measured()
		labels := make([]string, 0, len(measured))
		lines := make([]string, 0, len(measured))
		for _, s := range measured {
			labels = append(labels, s.component+" "+s.version)
			if sum, ok := p.Sysexts[s.component][s.version]; ok {
				lines = append(lines, SysextMeasurement(sum, s.component+"_"+s.version+".raw"))
			}
		}
		label := strings.Join(labels, ", ")
		if len(lines) == len(measured) {
			return SysextPCR(lines...), label, true
		}
		if v, ok := p.PCR13[p.KubernetesVersion]; ok && p.CNIVersion == "" {
			return strings.ToLower(v), label, true
		}
	}
	want, ok = p.golden(index)
	return want, "", ok
}

// sysext is one merged sysext: its component and version.
type sysext struct{ component, version string }

// measured lists the sysexts vm-kubernetes.service measures into PCR 13 on
// the VM, in its order.
func (p Policy) measured() []sysext {
	out := []sysext{{SysextKubernetes, p.KubernetesVersion}}
	if p.CNIVersion != "" {
		out = append(out, sysext{SysextCNI, p.CNIVersion})
	}
	return out
}

// PhaseFor is the phase path PCR 11 carries at a stage.
func PhaseFor(stage imds.Stage) string {
	if stage == imds.StageReady {
		return PhaseReady
	}
	return PhaseInitrd
}

// PCR15LayerCertificates is Policy.PCR15 of an image whose initrd extends
// PCR 15 with one layercert.Word per installation layer certificate.
const PCR15LayerCertificates = "layer-certificates"

// MeasuresLayerCertificates reports whether the image measures layer
// certificates into PCR 15 (Policy.PCR15).
func (p Policy) MeasuresLayerCertificates() bool {
	return p.PCR15 == PCR15LayerCertificates
}

// RequiredPCRs are the indexes every quote of a stage must cover: 0-7 and
// 11, plus 13 at the ready stage. The ready quote of an image that measures
// layer certificates must also cover PCR 15 (Policy.RequiredPCRs).
func RequiredPCRs(stage imds.Stage) []int {
	pcrs := append(append([]int(nil), firmwarePCRs...), PCRUKI)
	if stage == imds.StageReady {
		pcrs = append(pcrs, PCRSysext)
	}
	return pcrs
}

// RequiredPCRs are the indexes the image's quote of a stage must cover:
// RequiredPCRs, plus PCR 15 at the ready stage when the image measures layer
// certificates, whether or not the VM is served any: the guest picks the
// PCRs it quotes, so "none served" is proven as "none measured". An image
// that predates layer certificates may leave PCR 15 out.
func (p Policy) RequiredPCRs(stage imds.Stage) []int {
	pcrs := RequiredPCRs(stage)
	if stage == imds.StageReady && p.MeasuresLayerCertificates() {
		pcrs = append(pcrs, PCRSystemIdentity)
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
