// Package e2e holds the T3 boot end-to-end tests of docs/design.md "Testing
// strategy": real QEMU on KVM, swtpm, OVMF and the mkosi image from images/.
// TestInstallBoot drives them through internal/runtime/qemu and internal/tpm
// with user networking; the others (TestNetworkIMDS, TestIgnition,
// TestPersistentEtc, TestKubernetesSysext, TestAttestation) build and start
// vm-manager serve and drive it through its MCP endpoint, so the guest gets a
// virtual network with the IMDS; TestAttestation runs three servers in turn
// (learn, golden, tampered firmware) against a private copy of the image
// directory's policy.json.
//
// The tests are behind the e2e build tag so that go test ./... and go vet ./...
// stay fast and hermetic:
//
//	make e2e                         # go test -tags e2e -count=1 -timeout 45m -v ./e2e/...
//	VM_MANAGER_E2E_IMAGE_DIR=... make e2e
//
// They need /dev/kvm and /dev/vhost-vsock, qemu-system-x86_64, swtpm, the
// edk2 OVMF firmware, sfdisk, ssh with systemd-ssh-proxy, and the image
// artifacts built by `make -C images keys base` (giantswarm-vm-base_<v>.efi
// and .raw in images/build, or in $VM_MANAGER_E2E_IMAGE_DIR). A host without
// them skips with a message saying what is missing.
//
// On failure the per-test state directory (serial consoles, TPM state, target
// disk) is kept and its path printed; VM_MANAGER_E2E_KEEP=1 keeps it always.
package e2e
