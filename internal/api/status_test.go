package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/vm"
)

func TestStatusFor(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: vm x", apierr.ErrNotFound), http.StatusNotFound, "not_found"},
		{fmt.Errorf("%w: name required", apierr.ErrInvalid), http.StatusBadRequest, "invalid_request"},
		{fmt.Errorf("%w: vm running", apierr.ErrConflict), http.StatusConflict, "conflict"},
		{fmt.Errorf("%w: no kvm", apierr.ErrUnsupported), http.StatusNotImplemented, "unsupported"},
		{fmt.Errorf("%w: ready not reached", vm.ErrTimeout), http.StatusGatewayTimeout, "timeout"},
		{fmt.Errorf("%w: installer exited 1", vm.ErrFailed), http.StatusInternalServerError, "vm_failed"},
		{errors.New("qmp: connection reset"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			status, code := statusFor(tt.err)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.code, code)
			res := errResult(tt.err)
			assert.True(t, res.IsError)
			assert.Contains(t, fmt.Sprint(res.Content[0]), tt.code+": ", "MCP errors carry the REST code")
		})
	}
}

func TestCreateVMRequestDefaults(t *testing.T) {
	s := CreateVMRequest{Name: "n"}.spec()
	assert.Equal(t, DefaultCPUs, s.CPUs)
	assert.Equal(t, DefaultMemoryMiB, s.MemoryMiB)
	assert.Equal(t, DefaultDiskGiB, s.DiskGiB)
	assert.Equal(t, DefaultNetwork, s.Network)
	assert.True(t, s.RequireAttestation)
	assert.Equal(t, vm.WaitReady, s.WaitFor)
	assert.Nil(t, s.UserData)

	off := false
	s = CreateVMRequest{Name: "n", RequireAttestation: &off, WaitFor: "none", UserData: "{}", CPUs: 4}.spec()
	assert.False(t, s.RequireAttestation)
	assert.Equal(t, vm.WaitNone, s.WaitFor)
	assert.Equal(t, []byte("{}"), s.UserData)
	assert.Equal(t, 4, s.CPUs)
}
