package vm_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/vm"
)

func TestStateDirLock(t *testing.T) {
	h := newHarness(t)
	opts := vm.Options{
		StateDir: h.stateDir, Images: h.imgs, Storage: h.store, TPM: h.tpm, Runtime: h.rt, Networks: h.nets, Notify: h.notify,
		OVMFCode: filepath.Join(h.imageDir, "OVMF_CODE.fd"), OVMFVarsTemplate: filepath.Join(h.imageDir, "OVMF_VARS.fd"), Logger: quiet(),
	}

	// The running service holds the lock and wrote its pid into it.
	data, err := os.ReadFile(filepath.Join(h.stateDir, vm.LockFile))
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(os.Getpid()), strings.TrimSpace(string(data)))
	_, err = vm.New(opts)
	require.ErrorIs(t, err, vm.ErrStateDirInUse)
	assert.ErrorContains(t, err, "held by pid "+strconv.Itoa(os.Getpid()))

	// Close releases it; a second service on another dir never contends.
	require.NoError(t, h.svc.Close(h.ctx))
	second, err := vm.New(opts)
	require.NoError(t, err)
	require.NoError(t, second.Close(h.ctx))
	opts.StateDir = t.TempDir()
	third, err := vm.New(opts)
	require.NoError(t, err)
	require.NoError(t, third.Close(h.ctx))
}
