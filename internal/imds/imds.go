package imds

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/netip"
	"path"
	"sync"
	"time"

	"github.com/giantswarm/vm-manager/internal/nonce"
)

// Instance is what the Resolver knows about the VM behind a client address.
// It is a plain value so the VM service can build it from its own state
// without importing anything from this package but the type.
type Instance struct {
	// ID is vm-manager's VM ID, served as /instance-id and used to address
	// the Attestor and the ReportSink.
	ID string
	// Name is the VM name; it stands in for Hostname when that is empty.
	Name string
	// Hostname is served as /hostname.
	Hostname string
	// Region is the host name, served as /region.
	Region string
	// Zone is the network name, served as /zone.
	Zone string
	// SSHAuthorizedKeys are served as /public-keys/<n>; key 0 becomes the
	// guest's root authorized key.
	SSHAuthorizedKeys []string
	// KubernetesVersion is served as /kubernetes-version.
	KubernetesVersion string
	// Metadata is served as /metadata/<k>.
	Metadata map[string]string
	// UserData is the CAPI bootstrap data (Ignition JSON), served as
	// /user-data once released.
	UserData []byte
	// UserDataReleased is set by the VM service once an initrd-stage quote
	// verified (see ReleasesUserData).
	UserDataReleased bool
}

// Resolver maps the client address of a request to the VM it belongs to.
// The VM service implements it from the addresses it handed out.
type Resolver interface {
	LookupByIP(ctx context.Context, ip netip.Addr) (Instance, bool)
}

// Stage is the boot phase a quote is taken in.
type Stage string

// Stages the guest agent quotes at.
const (
	// StageInitrd is quoted from the initrd before user-data is fetched; it
	// is the only stage that releases /user-data.
	StageInitrd Stage = "initrd"
	// StageReady is quoted once the system is up and is recorded for
	// get_vm_attestation.
	StageReady Stage = "ready"
)

// Valid reports whether s is a stage the contract knows.
func (s Stage) Valid() bool {
	return s == StageInitrd || s == StageReady
}

// QuoteRequest is the body of POST /attest/quote. Binary fields are base64
// in JSON; PCRs is bank -> index -> hex digest, e.g. pcrs.sha256["11"].
type QuoteRequest struct {
	Stage        Stage                        `json:"stage"`
	Nonce        string                       `json:"nonce"`
	AKPub        []byte                       `json:"ak_pub"`
	EKPub        []byte                       `json:"ek_pub,omitempty"`
	Quote        []byte                       `json:"quote"`
	Signature    []byte                       `json:"signature"`
	PCRs         map[string]map[string]string `json:"pcrs"`
	EventLog     []byte                       `json:"event_log,omitempty"`
	UserspaceLog []byte                       `json:"userspace_log,omitempty"`
}

// QuoteResult is the body of the POST /attest/quote response. Verified says
// whether the Attestor accepted the quote; UserDataReleased whether that, per
// ReleasesUserData, unlocked /user-data.
type QuoteResult struct {
	Stage            Stage  `json:"stage"`
	Verified         bool   `json:"verified"`
	UserDataReleased bool   `json:"user_data_released"`
	Message          string `json:"message,omitempty"`
}

// ReleasesUserData is the gating rule: only a verified initrd-stage quote
// unlocks /user-data. The handler reports it in QuoteResult and the VM
// service applies it when it persists Instance.UserDataReleased.
func ReleasesUserData(stage Stage, verified bool) bool {
	return verified && stage == StageInitrd
}

// Attestor issues nonces and judges quotes. A rejected quote is a
// QuoteResult with Verified false, not an error; errors wrapping
// apierr.ErrInvalid answer 400, apierr.ErrNotFound 404, anything else 500.
// Implementations fill Verified and Message; the handler sets Stage and
// UserDataReleased (ReleasesUserData) so every Attestor answers by the same
// rule.
type Attestor interface {
	Nonce(ctx context.Context, vmID string) (string, error)
	SubmitQuote(ctx context.Context, vmID string, req QuoteRequest) (QuoteResult, error)
}

// ReportSink stores what systemd-report upload POSTs to /report.
type ReportSink interface {
	StoreReport(ctx context.Context, vmID string, report json.RawMessage) error
}

// ArtifactSource backs /sysupdate/<component>/<name>. Open returns
// fs.ErrNotExist (or fs.ErrInvalid) for anything that is not a regular file
// the guest may fetch; the handler turns that into 404.
type ArtifactSource interface {
	Open(component, name string) (fs.File, error)
}

// FSArtifacts serves component directories below one fs.FS, typically
// os.DirFS of the host's sysupdate tree: <root>/<component>/<name>.
type FSArtifacts struct {
	FS fs.FS
}

// Open implements ArtifactSource.
func (a FSArtifacts) Open(component, name string) (fs.File, error) {
	return a.FS.Open(path.Join(component, name))
}

// NonceTTL is how long a nonce from /attest/nonce stays valid, whichever
// Attestor issued it: every Attestor keeps its nonces in a nonce.Store with
// the defaults.
const NonceTTL = nonce.DefaultTTL

// NoopAttestor hands out nonces and accepts every quote that echoes an
// unexpired one back; it verifies nothing about the TPM. It exists for tests
// and bring-up before the verifier lands and must not back a production
// server. The zero value is ready to use.
type NoopAttestor struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	once   sync.Once
	nonces *nonce.Store
}

// Nonce implements Attestor with a nonce from the store.
func (a *NoopAttestor) Nonce(_ context.Context, vmID string) (string, error) {
	return a.store().Issue(vmID)
}

// SubmitQuote implements Attestor: the quote verifies iff its nonce was issued
// to this VM within NonceTTL. Nonces are single-use.
func (a *NoopAttestor) SubmitQuote(_ context.Context, vmID string, req QuoteRequest) (QuoteResult, error) {
	if !a.store().Consume(vmID, req.Nonce) {
		return QuoteResult{Message: "unknown or expired nonce"}, nil
	}
	return QuoteResult{Verified: true, Message: "nonce matched; quote not verified (noop attestor)"}, nil
}

// store builds the nonce store on first use so the zero value is ready; the
// store reads the clock through now, so Now needs no wiring.
func (a *NoopAttestor) store() *nonce.Store {
	a.once.Do(func() { a.nonces = nonce.New(nonce.WithClock(a.now)) })
	return a.nonces
}

func (a *NoopAttestor) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
