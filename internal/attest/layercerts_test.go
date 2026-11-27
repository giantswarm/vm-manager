package attest

import (
	"encoding/hex"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdata/tpm2-measure.log is what systemd-pcrextend 262 wrote for
//
//	systemd-pcrextend --pcr=15 --bank=sha256 layer-certificate:<a>
//	systemd-pcrextend --pcr=15 --bank=sha256 other:word
//	systemd-pcrextend --pcr=15 --bank=sha256 layer-certificate:<b>
//
// against a fresh swtpm (bootId zeroed), and systemdPCR15 what tpm2_pcrread
// read back from PCR 15 afterwards.
const (
	systemdCertA = "09284b891a68eb5cc2135d936570de408af1c1144115cd4b80842d818619522d"
	systemdCertB = "51c902768794f19b0b5120fefd2d66427563515c2e7c5acf5350a6c3307e3fc4"
	systemdPCR15 = "02c4b09d26cc2ce513da5ec76ee21f973fc4bc932c1386530c993957b03adfa0"
)

func TestMeasuredLayerCertificatesReadsSystemdLog(t *testing.T) {
	log, err := os.ReadFile("testdata/tpm2-measure.log")
	require.NoError(t, err)
	quoted, err := hex.DecodeString(systemdPCR15)
	require.NoError(t, err)

	got, err := measuredLayerCertificates(log, quoted)
	require.NoError(t, err)
	assert.Equal(t, []string{systemdCertA, systemdCertB}, got, "words in log order, other records replayed but not collected")

	_, err = measuredLayerCertificates(log[:len(log)/2], quoted)
	assert.ErrorContains(t, err, "userspace log record", "a truncated record does not parse")
}

func TestMeasuredLayerCertificatesZeroPCR(t *testing.T) {
	got, err := measuredLayerCertificates(nil, make([]byte, 32))
	require.NoError(t, err)
	assert.Empty(t, got, "an untouched pcr 15 needs no log")

	_, err = measuredLayerCertificates(nil, []byte{1})
	assert.ErrorContains(t, err, "the 0 pcr 15 records of the userspace log replay to 0000")
}

func TestCompareLayerCertificates(t *testing.T) {
	const a, b, c = "aa", "bb", "cc"
	for _, tc := range []struct {
		name             string
		served, measured []string
		want             string
	}{
		{name: "equal", served: []string{a, b}, measured: []string{a, b}},
		{name: "none", served: nil, measured: []string{}},
		{name: "missing", served: []string{a, b}, measured: []string{a}, want: "missing bb"},
		{name: "extra", served: []string{a}, measured: []string{a, c}, want: "not served cc"},
		{name: "measured twice", served: []string{a}, measured: []string{a, a}, want: "not served aa"},
		{name: "replaced", served: []string{a, b}, measured: []string{a, c}, want: "missing bb; not served cc"},
		{name: "reordered", served: []string{a, b}, measured: []string{b, a}, want: "measured in the order bb, aa, served aa, bb"},
		{name: "none measured", served: []string{a}, measured: nil, want: "missing aa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compareLayerCertificates(tc.served, tc.measured)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.want)
		})
	}
}
