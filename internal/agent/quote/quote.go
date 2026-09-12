// Package quote builds the TPM 2.0 quote the guest agent posts to the IMDS
// (docs/design.md "Attestation protocol"). It is the guest half of the
// contract whose host half is imds.QuoteRequest: every field the verifier
// reads is produced here, so the verifier's tests can drive this package
// against a TPM simulator instead of hand-crafting quotes.
//
// # Keys
//
// The attestation key is an ECC P-256 restricted signing key (AKTemplate)
// created once under the storage hierarchy's SRK (tpm2.ECCSRKTemplate) and
// made persistent at AKHandle. The vTPM state of a VM survives the installer
// boot, the installed boots and every reboot, so all quotes of one VM carry
// the same AK and the verifier can pin it on first use. The endorsement key
// public area comes from the TCG RSA-2048 EK template (tpm2.RSAEKTemplate) under
// the endorsement hierarchy, read from EKHandle when the TPM already
// persists one there; no EK certificate is handled.
//
// The vTPM has no hierarchy auth values set, so every command uses an empty
// password session.
//
// # Measurements
//
// Stage initrd quotes PCRs 0-7 and 11 (phase enter-initrd); stage ready
// adds PCR 13 for the merged system extensions. Only the sha256 bank is
// quoted. The firmware event log and systemd's userspace measurement log are
// attached when present; the initrd may not have either yet, which is not an
// error.
package quote

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"github.com/google/go-tpm/tpm2/transport/linuxtpm"
	"github.com/google/go-tpm/tpm2/transport/linuxudstpm"

	"github.com/giantswarm/vm-manager/internal/imds"
)

// Defaults for Options.
const (
	// DefaultDevice is the in-kernel resource manager of the vTPM.
	DefaultDevice = "/dev/tpmrm0"
	// DefaultFirmwareLog is the TCG event log the kernel exposes.
	DefaultFirmwareLog = "/sys/kernel/security/tpm0/binary_bios_measurements"
	// DefaultUserspaceLog is systemd's measurement log (pcrphase, pcrextend).
	DefaultUserspaceLog = "/run/log/systemd/tpm2-measure.log"
)

// Bank is the only PCR bank quoted; it keys imds.QuoteRequest.PCRs.
const Bank = "sha256"

// bankAlg is Bank as a TPM algorithm.
const bankAlg = tpm2.TPMAlgSHA256

// Options select the TPM and the logs. The zero value opens DefaultDevice
// and reads the default logs.
type Options struct {
	// TPM is an open transport; nil opens Device. Tests inject a simulator.
	TPM transport.TPM
	// Device is the TPM character device; empty means DefaultDevice.
	Device string
	// FirmwareLog is the firmware event log path; empty means
	// DefaultFirmwareLog. A missing file is skipped.
	FirmwareLog string
	// UserspaceLog is the systemd measurement log path; empty means
	// DefaultUserspaceLog. A missing file is skipped.
	UserspaceLog string
}

// Selection returns the PCR indexes quoted at stage, in ascending order.
func Selection(stage imds.Stage) ([]uint, error) {
	switch stage {
	case imds.StageInitrd:
		return []uint{0, 1, 2, 3, 4, 5, 6, 7, 11}, nil
	case imds.StageReady:
		return []uint{0, 1, 2, 3, 4, 5, 6, 7, 11, 13}, nil
	default:
		return nil, fmt.Errorf("unknown stage %q", stage)
	}
}

// Build produces the request for stage with nonce (hex, as served by
// /attest/nonce) as qualifying data. It ensures the persistent AK, reads the
// EK public area and the PCRs of Selection, quotes them and attaches the
// event logs.
func Build(opts Options, stage imds.Stage, nonce string) (imds.QuoteRequest, error) {
	pcrs, err := Selection(stage)
	if err != nil {
		return imds.QuoteRequest{}, err
	}
	qualifying, err := DecodeNonce(nonce)
	if err != nil {
		return imds.QuoteRequest{}, err
	}

	tpm, closeTPM, err := open(opts)
	if err != nil {
		return imds.QuoteRequest{}, err
	}
	defer closeTPM()

	ak, err := EnsureAK(tpm)
	if err != nil {
		return imds.QuoteRequest{}, fmt.Errorf("attestation key: %w", err)
	}
	ekPub, err := EKPublic(tpm)
	if err != nil {
		return imds.QuoteRequest{}, fmt.Errorf("endorsement key: %w", err)
	}
	values, err := ReadPCRs(tpm, bankAlg, pcrs)
	if err != nil {
		return imds.QuoteRequest{}, fmt.Errorf("read pcrs: %w", err)
	}
	attest, sig, err := ak.Quote(tpm, bankAlg, pcrs, qualifying)
	if err != nil {
		return imds.QuoteRequest{}, fmt.Errorf("quote: %w", err)
	}

	req := imds.QuoteRequest{
		Stage:     stage,
		Nonce:     nonce,
		AKPub:     ak.PublicBytes,
		EKPub:     ekPub,
		Quote:     attest,
		Signature: sig,
		PCRs:      map[string]map[string]string{Bank: hexPCRs(values)},
	}
	if isSocket(orDefault(opts.Device, DefaultDevice)) {
		// A socket is a swtpm on a development host; the host's own event
		// logs describe a different TPM and are left out.
		return req, nil
	}
	if req.EventLog, err = readOptional(orDefault(opts.FirmwareLog, DefaultFirmwareLog)); err != nil {
		return imds.QuoteRequest{}, err
	}
	if req.UserspaceLog, err = readOptional(orDefault(opts.UserspaceLog, DefaultUserspaceLog)); err != nil {
		return imds.QuoteRequest{}, err
	}
	return req, nil
}

// Open returns the TPM of opts: the injected transport, or the device opened
// for the caller to close. Device may also be the unix socket of a
// `swtpm socket --tpm2 --server type=unixio,path=...`, which is how the
// agent is exercised on a development host without a TPM device.
func Open(opts Options) (transport.TPMCloser, error) {
	if opts.TPM != nil {
		return nopCloser{opts.TPM}, nil
	}
	dev := orDefault(opts.Device, DefaultDevice)
	openDev := linuxtpm.Open
	if isSocket(dev) {
		openDev = linuxudstpm.Open
	}
	tpm, err := openDev(dev)
	if err != nil {
		return nil, fmt.Errorf("open tpm %s: %w", dev, err)
	}
	return tpm, nil
}

// isSocket reports whether path exists and is a unix socket.
func isSocket(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// open is Open with a cleanup func that ignores the close error.
func open(opts Options) (transport.TPM, func(), error) {
	tpm, err := Open(opts)
	if err != nil {
		return nil, nil, err
	}
	return tpm, func() { _ = tpm.Close() }, nil
}

// nopCloser lets an injected transport be used where a closer is expected
// without taking ownership of it.
type nopCloser struct{ transport.TPM }

func (nopCloser) Close() error { return nil }

// readOptional returns the file's bytes, or nil when it does not exist.
// maxEventLogBytes bounds one event log; real logs are tens of kilobytes,
// and the initrd has little memory to spare for a pathological one.
const maxEventLogBytes = 4 << 20

func readOptional(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- fixed kernel/systemd log paths, overridden only by tests
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read event log %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxEventLogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read event log %s: %w", path, err)
	}
	if len(data) > maxEventLogBytes {
		return nil, fmt.Errorf("read event log %s: larger than %d bytes", path, maxEventLogBytes)
	}
	return data, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
