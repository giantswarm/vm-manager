//go:build e2e

package e2e

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// binaryField is name and value in the export format's length-prefixed form.
func binaryField(name, value string) string {
	var b bytes.Buffer
	b.WriteString(name + "\n")
	_ = binary.Write(&b, binary.LittleEndian, uint64(len(value)))
	b.WriteString(value + "\n")
	return b.String()
}

// TestCopyJournal renders a stream as journald's ForwardToSocket= writes it:
// text and binary fields, a multi-line message, a boot change, and a stream
// that ends inside an entry when the guest dies.
func TestCopyJournal(t *testing.T) {
	stream := "__CURSOR=s=1\n__MONOTONIC_TIMESTAMP=1234567\n_BOOT_ID=b1\nSYSLOG_IDENTIFIER=systemd-networkd\n_PID=180\nMESSAGE=eth0: Link UP\n\n" +
		"__MONOTONIC_TIMESTAMP=2000001\n_BOOT_ID=b1\n_COMM=systemd-networkd-wait-online\n_PID=190\n" + binaryField("MESSAGE", "line one\nline two") + "\n" +
		"__MONOTONIC_TIMESTAMP=5\n_BOOT_ID=b2\nSYSLOG_IDENTIFIER=kernel\nMESSAGE=Linux version\n\n" +
		"__MONOTONIC_TIMESTAMP=6\n_BOOT_ID=b2\nMESSAGE=cut"

	var out strings.Builder
	err := copyJournal(&out, strings.NewReader(stream))
	require.Error(t, err, "a stream ending inside an entry")
	assert.Equal(t, "-- Boot b1 --\n"+
		"[    1.234567] systemd-networkd[180]: eth0: Link UP\n"+
		"[    2.000001] systemd-networkd-wait-online[190]: line one\n    line two\n"+
		"-- Boot b2 --\n"+
		"[    0.000005] kernel: Linux version\n"+
		"[    0.000006] ?: cut\n", out.String())
}

// TestCopyJournalRejectsOversizedField stops at a length no journal entry
// has instead of allocating it.
func TestCopyJournalRejectsOversizedField(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("MESSAGE\n")
	_ = binary.Write(&b, binary.LittleEndian, uint64(journalMaxField+1))
	err := copyJournal(&strings.Builder{}, &b)
	require.ErrorContains(t, err, "bytes")
}
