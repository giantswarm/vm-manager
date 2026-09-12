package imds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHwdbRecordParity pins the image's provider record to the Go key table:
// change Keys, then write HwdbRecord() to HwdbFile.
func TestHwdbRecordParity(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", HwdbFile))
	require.NoError(t, err)
	assert.Equal(t, string(shipped), HwdbRecord(),
		"regenerate %s from imds.HwdbRecord()", HwdbFile)
}

func TestHwdbRecordShape(t *testing.T) {
	rec := HwdbRecord()
	assert.Contains(t, rec, "\n"+HwdbMatch+"\n")
	assert.Contains(t, rec, " IMDS_DATA_URL="+DataURL+"\n")
	assert.NotContains(t, rec, "IMDS_TOKEN_URL", "the contract has no token flow")
	assert.False(t, strings.HasSuffix(DataURL, "/"), "IMDS_DATA_URL must not end in /")
	for _, k := range Keys {
		if k.Property != "" {
			assert.Contains(t, rec, " "+k.Property+"="+k.Path+"\n")
		}
	}
}

func TestKeysRouted(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range Keys {
		assert.True(t, strings.HasPrefix(k.Path, "/"), "%q must begin with /", k.Path)
		assert.NotEmpty(t, k.Method, k.Path)
		assert.NotEmpty(t, k.Description, k.Path)
		assert.Contains(t, routes, k.Path, "no handler for key")
		assert.False(t, seen[k.Path], "duplicate key %q", k.Path)
		seen[k.Path] = true
	}
	for path := range routes {
		assert.True(t, seen[path], "handler %q is not in Keys", path)
	}
}
