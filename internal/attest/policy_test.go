package attest

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/imds"
	"github.com/giantswarm/vm-manager/internal/tpmquote/quotetest"
)

const (
	hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestParsePolicy(t *testing.T) {
	valid := `{"image_id":"giantswarm-vm-base","image_version":"0.1.0","uki":"x.efi","roothash":"r","partitions":{"esp":"u"},
		"pcr11":{"enter-initrd":"` + hexA + `","enter-initrd:leave-initrd:sysinit:ready":"` + hexB + `"},
		"pcr13":{"1.36.4":"` + hexB + `"},"pcr15":"layer-certificates",
		"sysexts":{"kubernetes":{"1.36.4":"` + hexA + `"},"cni":{"cilium-1.19.4":"` + hexB + `"}},
		"golden":{"sha256":{"0":"` + hexA + `","7":"` + hexB + `","13":"` + hexA + `"}}}`
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "verify script output plus golden", raw: valid},
		{name: "no golden", raw: `{"pcr11":{"enter-initrd":"` + hexA + `","enter-initrd:leave-initrd:sysinit:ready":"` + hexB + `"}}`},
		{name: "not json", raw: `nope`, wantErr: "policy:"},
		{name: "missing ready phase", raw: `{"pcr11":{"enter-initrd":"` + hexA + `"}}`, wantErr: `pcr11 lacks phase "enter-initrd:leave-initrd:sysinit:ready"`},
		{name: "pcr11 not hex", raw: `{"pcr11":{"enter-initrd":"zz","enter-initrd:leave-initrd:sysinit:ready":"` + hexB + `"}}`, wantErr: "not hex"},
		{name: "pcr11 short", raw: `{"pcr11":{"enter-initrd":"abcd","enter-initrd:leave-initrd:sysinit:ready":"` + hexB + `"}}`, wantErr: "2 bytes, want 32"},
		{name: "golden bank", raw: strings.Replace(valid, `"golden":{"sha256"`, `"golden":{"sha1"`, 1), wantErr: `golden bank "sha1" is not supported`},
		{name: "golden index", raw: strings.Replace(valid, `"13":`, `"24":`, 1), wantErr: "pcr index 24 out of range"},
		{name: "golden index not a number", raw: strings.Replace(valid, `"13":`, `"pcr13":`, 1), wantErr: "policy:"},
		{name: "golden value", raw: strings.Replace(valid, `"7":"`+hexB, `"7":"`+hexB[:10], 1), wantErr: "golden.sha256[7]"},
		{name: "pcr13 value", raw: strings.Replace(valid, `"1.36.4":"`+hexB, `"1.36.4":"zz`, 1), wantErr: `pcr13["1.36.4"]: not hex`},
		{name: "golden firmware", raw: strings.Replace(valid, `"golden":`, `"golden_firmware":{"sha256":"`+hexB+`","package":"ovmf-generic","version":"2025.11-3ubuntu7.2"},"golden":`, 1)},
		{name: "golden firmware digest", raw: strings.Replace(valid, `"golden":`, `"golden_firmware":{"sha256":"abcd"},"golden":`, 1), wantErr: "golden_firmware.sha256: 2 bytes, want 32"},
		{name: "sysexts value", raw: strings.Replace(valid, `"cilium-1.19.4":"`+hexB, `"cilium-1.19.4":"`+hexB[:8], 1), wantErr: `sysexts.cni["cilium-1.19.4"]: 4 bytes, want 32`},
		{name: "sysexts version", raw: strings.Replace(valid, `"cilium-1.19.4":`, `"":`, 1), wantErr: "sysexts has an entry without a component or version"},
		{name: "pcr13 version", raw: strings.Replace(valid, `"1.36.4":`, `"":`, 1), wantErr: "pcr13 has an entry without a kubernetes version"},
		{name: "pcr15 unknown", raw: strings.Replace(valid, `"pcr15":"layer-certificates"`, `"pcr15":"machine-id"`, 1), wantErr: `pcr15 "machine-id" is not "layer-certificates"`},
		{name: "image predating pcr15", raw: strings.Replace(valid, `"pcr15":"layer-certificates",`, ``, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParsePolicy([]byte(tt.raw))
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, hexA, p.PCR11[PhaseInitrd])
			assert.Equal(t, hexB, p.PCR11[PhaseReady])
		})
	}

	p, err := ParsePolicy([]byte(valid))
	require.NoError(t, err)
	assert.Equal(t, "giantswarm-vm-base", p.ImageID)
	assert.True(t, p.MeasuresLayerCertificates())
	assert.Equal(t, map[int]string{0: hexA, 7: hexB, 13: hexA}, p.Golden[Bank])
	v, ok := p.golden(7)
	assert.True(t, ok)
	assert.Equal(t, hexB, v)
	_, ok = p.golden(3)
	assert.False(t, ok)
	assert.Equal(t, map[string]string{"1.36.4": hexB}, p.PCR13)
	assert.Empty(t, p.KubernetesVersion, "not part of policy.json")
	assert.Nil(t, p.GoldenFirmware, "values recorded before the firmware was")
}

func TestFirmware(t *testing.T) {
	fw := Firmware{SHA256: hexA, Package: "ovmf-generic", Version: "2025.11-3ubuntu7.2"}
	assert.Equal(t, "ovmf-generic 2025.11-3ubuntu7.2 (sha256 aaaaaaaaaaaaaaaa)", fw.String())
	assert.Equal(t, "sha256 bbbbbbbbbbbbbbbb", Firmware{SHA256: hexB}.String(), "a build no package manager knows")
	assert.True(t, fw.SameBuild(Firmware{SHA256: strings.ToUpper(hexA)}), "the digest decides, in any case")
	assert.False(t, fw.SameBuild(Firmware{SHA256: hexB, Package: fw.Package, Version: fw.Version}))
}

// The PCR 13 expectation: the pcr13 entry of the VM's Kubernetes version,
// else golden 13; every other index is golden only.
func TestPolicyExpected(t *testing.T) {
	p := Policy{
		PCR13:  map[string]string{"1.36.4": strings.ToUpper(hexB)},
		Golden: map[string]map[int]string{Bank: {0: hexA, 13: hexA}},
	}
	want, sysexts, ok := p.expected(13)
	assert.True(t, ok)
	assert.Equal(t, hexA, want, "no sysexts version: golden")
	assert.Empty(t, sysexts)

	p.KubernetesVersion = "1.36.4"
	want, sysexts, ok = p.expected(13)
	assert.True(t, ok)
	assert.Equal(t, hexB, want, "lowercased pcr13 entry")
	assert.Equal(t, "kubernetes 1.36.4", sysexts)
	want, sysexts, ok = p.expected(0)
	assert.True(t, ok)
	assert.Equal(t, hexA, want)
	assert.Empty(t, sysexts)

	p.KubernetesVersion = "1.35.0"
	want, sysexts, ok = p.expected(13)
	assert.True(t, ok)
	assert.Equal(t, hexA, want, "no entry for the version: golden")
	assert.Empty(t, sysexts)

	delete(p.Golden[Bank], 13)
	_, _, ok = p.expected(13)
	assert.False(t, ok)
}

// SysextPCR is what a TPM computes when the initrd extends PCR 13 with
// "os-separator" and vm-kubernetes.service with the sha256sum line of each
// sysext it merged (the one-sysext vector comes from the Python rule
// images/scripts/verify-sysext implemented before policy.json carried
// sysexts).
func TestSysextPCR(t *testing.T) {
	line := SysextMeasurement(strings.ToUpper(hexA), "kubernetes_1.36.4.raw")
	assert.Equal(t, hexA+"  kubernetes_1.36.4.raw", line)
	assert.Equal(t, "7f142c6d48ce7a4e2b3d88cb699ae982e3478622d9078bd562a7aff58f1846bd", SysextPCR(line))

	pcr := make([]byte, 32)
	pcr = quotetest.Extended(pcr, OSSeparator)
	assert.Equal(t, "3345a4e7857aa5ae65e97702ade84a3755fd6144724779536b5773128676c99c", hex.EncodeToString(pcr), "golden 13 of a boot without a sysext")
	pcr = quotetest.Extended(pcr, line)
	assert.Equal(t, hex.EncodeToString(pcr), SysextPCR(line))
	require.NoError(t, Policy{PCR11: map[string]string{PhaseInitrd: hexA, PhaseReady: hexB}, PCR13: map[string]string{"1.36.4": SysextPCR(line)}}.Validate())
}

// PCR 13 of every Kubernetes x CNI combination is computed from one entry
// per sysext, in the order vm-kubernetes.service measures them; a VM whose
// versions lack an entry falls back to pcr13 (without CNI only), then to
// golden.
func TestExpectedSysexts(t *testing.T) {
	sums := map[string]string{
		"kubernetes_1.36.4.raw": hexA,
		"kubernetes_1.35.4.raw": hexB,
		"cni_cilium-1.19.4.raw": strings.Repeat("c", 64),
		"cni_cilium-1.20.5.raw": strings.Repeat("d", 64),
		"cni_calico-3.31.0.raw": strings.Repeat("e", 64),
	}
	p := Policy{
		PCR11: map[string]string{PhaseInitrd: hexA, PhaseReady: hexB},
		Sysexts: map[string]map[string]string{
			SysextKubernetes: {"1.36.4": hexA, "1.35.4": strings.ToUpper(hexB)},
			SysextCNI:        {"cilium-1.19.4": sums["cni_cilium-1.19.4.raw"], "cilium-1.20.5": sums["cni_cilium-1.20.5.raw"], "calico-3.31.0": sums["cni_calico-3.31.0.raw"]},
		},
		Golden: map[string]map[int]string{Bank: {13: hexB}},
	}
	require.NoError(t, p.Validate())
	tpm := func(files ...string) string {
		pcr := quotetest.Extended(make([]byte, 32), OSSeparator)
		for _, f := range files {
			pcr = quotetest.Extended(pcr, SysextMeasurement(sums[f], f))
		}
		return hex.EncodeToString(pcr)
	}
	for _, k := range []string{"1.36.4", "1.35.4"} {
		for _, c := range []string{"cilium-1.19.4", "cilium-1.20.5", "calico-3.31.0"} {
			p.KubernetesVersion, p.CNIVersion = k, c
			want, label, ok := p.expected(PCRSysext)
			require.True(t, ok)
			assert.Equal(t, tpm("kubernetes_"+k+".raw", "cni_"+c+".raw"), want, "%s x %s", k, c)
			assert.Equal(t, "kubernetes "+k+", cni "+c, label)
		}
	}

	p.KubernetesVersion, p.CNIVersion = "1.36.4", ""
	want, label, ok := p.expected(PCRSysext)
	require.True(t, ok)
	assert.Equal(t, tpm("kubernetes_1.36.4.raw"), want, "kubernetes without cni")
	assert.Equal(t, "kubernetes 1.36.4", label)

	p.CNIVersion = "cilium-0.1.0"
	p.PCR13 = map[string]string{"1.36.4": hexA}
	want, label, _ = p.expected(PCRSysext)
	assert.Equal(t, hexB, want, "unpublished cni: golden, never the cni-less pcr13 entry")
	assert.Empty(t, label)

	p.CNIVersion = ""
	p.Sysexts = nil
	want, label, _ = p.expected(PCRSysext)
	assert.Equal(t, hexA, want, "policy of an image without sysexts: pcr13 entry")
	assert.Equal(t, "kubernetes 1.36.4", label)

	want, _, _ = p.expected(0)
	assert.Empty(t, want, "only pcr 13 is computed")
}

func TestPolicyHelpers(t *testing.T) {
	assert.Equal(t, PhaseInitrd, PhaseFor(imds.StageInitrd))
	assert.Equal(t, PhaseReady, PhaseFor(imds.StageReady))
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 11}, RequiredPCRs(imds.StageInitrd))
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 11, 13}, RequiredPCRs(imds.StageReady))
	assert.Equal(t, RequiredPCRs(imds.StageReady), Policy{LayerCertificates: []string{hexA}}.RequiredPCRs(imds.StageReady), "an image without pcr15 need not quote it")
	measuring := Policy{PCR15: PCR15LayerCertificates}
	assert.True(t, measuring.MeasuresLayerCertificates())
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 11, 13, 15}, measuring.RequiredPCRs(imds.StageReady), "required with or without certificates served")
	assert.Equal(t, RequiredPCRs(imds.StageInitrd), measuring.RequiredPCRs(imds.StageInitrd), "pcr 15 is a ready-stage measurement")
	assert.Equal(t, []int{0, 2, 3, 4, 6, 7, 13}, GoldenIndexes, "pcr 1 (smbios, boot entry) and 5 (gpt) differ per vm and are not golden")

	pcrs := map[int]string{}
	for _, i := range RequiredPCRs(imds.StageReady) {
		pcrs[i] = hexA
	}
	golden := GoldenFromPCRs(pcrs)
	assert.Len(t, golden, len(GoldenIndexes))
	assert.NotContains(t, golden, 11, "pcr 11 comes from the uki, not from a golden boot")
	assert.Equal(t, hexA, golden[13])
	assert.Equal(t, "0,4,13", indexList([]int{13, 4, 0}))
}
