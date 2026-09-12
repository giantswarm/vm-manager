package attest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/imds"
)

const (
	hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestParsePolicy(t *testing.T) {
	valid := `{"image_id":"giantswarm-vm-base","image_version":"0.1.0","uki":"x.efi","roothash":"r","partitions":{"esp":"u"},
		"pcr11":{"enter-initrd":"` + hexA + `","enter-initrd:leave-initrd:sysinit:ready":"` + hexB + `"},
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
}

func TestPolicyHelpers(t *testing.T) {
	assert.Equal(t, PhaseInitrd, PhaseFor(imds.StageInitrd))
	assert.Equal(t, PhaseReady, PhaseFor(imds.StageReady))
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 11}, RequiredPCRs(imds.StageInitrd))
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 11, 13}, RequiredPCRs(imds.StageReady))
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 13}, GoldenIndexes)

	pcrs := map[int]string{}
	for _, i := range RequiredPCRs(imds.StageReady) {
		pcrs[i] = hexA
	}
	golden := GoldenFromPCRs(pcrs)
	assert.Len(t, golden, 9)
	assert.NotContains(t, golden, 11, "pcr 11 comes from the uki, not from a golden boot")
	assert.Equal(t, hexA, golden[13])
	assert.Equal(t, "0,4,13", indexList([]int{13, 4, 0}))
}
