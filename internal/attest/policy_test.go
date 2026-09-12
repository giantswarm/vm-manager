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
		"pcr13":{"1.36.4":"` + hexB + `"},
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
		{name: "pcr13 version", raw: strings.Replace(valid, `"1.36.4":`, `"":`, 1), wantErr: "pcr13 has an entry without a kubernetes version"},
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
	assert.Equal(t, map[int]string{0: hexA, 7: hexB, 13: hexA}, p.Golden[Bank])
	v, ok := p.golden(7)
	assert.True(t, ok)
	assert.Equal(t, hexB, v)
	_, ok = p.golden(3)
	assert.False(t, ok)
	assert.Equal(t, map[string]string{"1.36.4": hexB}, p.PCR13)
	assert.Empty(t, p.KubernetesVersion, "not part of policy.json")
}

// The PCR 13 expectation: the pcr13 entry of the VM's Kubernetes version,
// else golden 13; every other index is golden only.
func TestPolicyExpected(t *testing.T) {
	p := Policy{
		PCR13:  map[string]string{"1.36.4": strings.ToUpper(hexB)},
		Golden: map[string]map[int]string{Bank: {0: hexA, 13: hexA}},
	}
	want, kubernetes, ok := p.expected(13)
	assert.True(t, ok)
	assert.Equal(t, hexA, want, "no kubernetes version: golden")
	assert.Empty(t, kubernetes)

	p.KubernetesVersion = "1.36.4"
	want, kubernetes, ok = p.expected(13)
	assert.True(t, ok)
	assert.Equal(t, hexB, want, "lowercased pcr13 entry")
	assert.Equal(t, "1.36.4", kubernetes)
	want, kubernetes, ok = p.expected(0)
	assert.True(t, ok)
	assert.Equal(t, hexA, want)
	assert.Empty(t, kubernetes)

	p.KubernetesVersion = "1.35.0"
	want, kubernetes, ok = p.expected(13)
	assert.True(t, ok)
	assert.Equal(t, hexA, want, "no entry for the version: golden")
	assert.Empty(t, kubernetes)

	delete(p.Golden[Bank], 13)
	_, _, ok = p.expected(13)
	assert.False(t, ok)
}

// SysextPCR is the rule images/scripts/verify-kubernetes implements in
// Python (the vector below comes from it) and what a TPM computes when the
// initrd extends PCR 13 with "os-separator" and vm-kubernetes.service with
// the sha256sum line of the sysext.
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

func TestPolicyHelpers(t *testing.T) {
	assert.Equal(t, PhaseInitrd, PhaseFor(imds.StageInitrd))
	assert.Equal(t, PhaseReady, PhaseFor(imds.StageReady))
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 11}, RequiredPCRs(imds.StageInitrd))
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 11, 13}, RequiredPCRs(imds.StageReady))
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
