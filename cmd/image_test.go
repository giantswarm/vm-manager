package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/attest"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/vm"
)

func TestImageGolden(t *testing.T) {
	hexOf := func(i int) string { return strings.Repeat(string("0123456789abcdef"[i%16]), 64) }
	imageDir := t.TempDir()
	for _, f := range []string{"giantswarm-vm-base_0.1.0.efi", "giantswarm-vm-base_0.1.0.raw"} {
		require.NoError(t, os.WriteFile(filepath.Join(imageDir, f), []byte(f), 0o600))
	}
	policyPath := filepath.Join(imageDir, "policy.json")
	policy := `{"image_id":"giantswarm-vm-base","image_version":"0.1.0","uki":"giantswarm-vm-base_0.1.0.efi",
		"pcr11":{"enter-initrd":"` + hexOf(1) + `","enter-initrd:leave-initrd:sysinit:ready":"` + hexOf(2) + `"}}`
	require.NoError(t, os.WriteFile(policyPath, []byte(policy), 0o600))

	pcrs := map[int]string{}
	for _, i := range attest.RequiredPCRs("ready") {
		pcrs[i] = hexOf(i)
	}
	attestations := map[string]vm.Attestation{
		"vm-ready":  {Required: true, UserDataReleased: true, Ready: &vm.Quote{Verified: true, AKFingerprint: "ak-fp", PCRs: pcrs}},
		"vm-initrd": {Required: true, UserDataReleased: true, Initrd: &vm.Quote{Verified: true}},
		"vm-noop":   {Required: true, UserDataReleased: true, Ready: &vm.Quote{Verified: true}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, api.Prefix+"/vms/"), "/attestation")
		att, ok := attestations[id]
		if !ok {
			http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(att)
	}))
	t.Cleanup(srv.Close)

	opts := func(vmID string) *imageGoldenOptions {
		return &imageGoldenOptions{fromVM: vmID, server: srv.URL + "/", token: "tok", imageDir: imageDir}
	}
	ctx := context.Background()

	err := runImageGolden(ctx, opts("vm-initrd"), "giantswarm-vm-base", io.Discard)
	assert.ErrorContains(t, err, "no verified ready-stage quote (initrd verified: true)")
	err = runImageGolden(ctx, opts("vm-noop"), "giantswarm-vm-base", io.Discard)
	assert.ErrorContains(t, err, "carries 0 of the 9 golden PCRs")
	err = runImageGolden(ctx, opts("vm-gone"), "giantswarm-vm-base", io.Discard)
	assert.ErrorContains(t, err, "404")
	err = runImageGolden(ctx, opts("vm-ready"), "other-image", io.Discard)
	assert.ErrorContains(t, err, "other-image")
	o := opts("vm-ready")
	o.token = ""
	assert.ErrorContains(t, runImageGolden(ctx, o, "giantswarm-vm-base", io.Discard), "401")

	var out bytes.Buffer
	require.NoError(t, runImageGolden(ctx, opts("vm-ready"), "giantswarm-vm-base", &out))
	assert.Equal(t, "recorded golden sha256 PCRs 0,1,2,3,4,5,6,7,13 of vm vm-ready (ak ak-fp) into "+policyPath+"\n", out.String())

	raw, err := os.ReadFile(policyPath) // #nosec G304 -- test temp dir.
	require.NoError(t, err)
	p, err := attest.ParsePolicy(raw)
	require.NoError(t, err)
	assert.Len(t, p.Golden[attest.Bank], 9)
	assert.Equal(t, hexOf(13), p.Golden[attest.Bank][13])
	assert.Equal(t, hexOf(1), p.PCR11[attest.PhaseInitrd], "the rest of the file is kept")
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &doc))
	assert.JSONEq(t, `"giantswarm-vm-base_0.1.0.efi"`, string(doc["uki"]))
	_, err = os.Stat(policyPath + ".tmp")
	assert.True(t, os.IsNotExist(err))

	// The catalog hands the completed policy to the verifier.
	cat, err := images.Load(imageDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	img, err := cat.Get("giantswarm-vm-base")
	require.NoError(t, err)
	assert.Equal(t, policyPath, cat.PolicyPath(img))
	assert.Contains(t, string(img.Policy), `"golden"`)

	// An image without any policy is refused rather than given a bare one.
	require.NoError(t, os.Remove(policyPath))
	assert.ErrorContains(t, runImageGolden(ctx, opts("vm-ready"), "giantswarm-vm-base", io.Discard), "has no policy.json")
}
