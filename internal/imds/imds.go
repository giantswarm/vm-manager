package imds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/netip"
	"path"
	"sync"
	"time"
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

// NonceTTL is how long a nonce from /attest/nonce stays valid.
const NonceTTL = 5 * time.Minute

// maxNoncesPerVM bounds outstanding nonces for one VM so a guest that keeps
// asking for nonces without ever quoting cannot grow the map without limit;
// the oldest nonce is dropped when the cap is reached.
const maxNoncesPerVM = 8

// NoopAttestor hands out nonces and accepts every quote that echoes an
// unexpired one back; it verifies nothing about the TPM. It exists for tests
// and bring-up before the verifier lands and must not back a production
// server. The zero value is ready to use. Expired nonces are swept on every
// call and at most maxNoncesPerVM are kept per VM.
type NoopAttestor struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu     sync.Mutex
	nonces map[string]map[string]time.Time // vmID -> nonce -> issued
}

// Nonce implements Attestor with 32 random bytes, hex-encoded.
func (a *NoopAttestor) Nonce(_ context.Context, vmID string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(raw[:])

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.nonces == nil {
		a.nonces = make(map[string]map[string]time.Time)
	}
	if a.nonces[vmID] == nil {
		a.nonces[vmID] = make(map[string]time.Time)
	}
	now := a.now()
	a.sweepLocked(now)
	for len(a.nonces[vmID]) >= maxNoncesPerVM {
		delete(a.nonces[vmID], oldestNonce(a.nonces[vmID]))
	}
	a.nonces[vmID][nonce] = now
	return nonce, nil
}

// sweepLocked drops expired nonces and empty per-VM maps; callers hold a.mu.
func (a *NoopAttestor) sweepLocked(now time.Time) {
	for vmID, issued := range a.nonces {
		for nonce, t := range issued {
			if now.Sub(t) > NonceTTL {
				delete(issued, nonce)
			}
		}
		if len(issued) == 0 {
			delete(a.nonces, vmID)
		}
	}
}

// oldestNonce returns the key with the earliest issue time; issued is not empty.
func oldestNonce(issued map[string]time.Time) string {
	var oldest string
	var oldestAt time.Time
	for nonce, t := range issued {
		if oldest == "" || t.Before(oldestAt) {
			oldest, oldestAt = nonce, t
		}
	}
	return oldest
}

// SubmitQuote implements Attestor: the quote verifies iff its nonce was issued
// to this VM within NonceTTL. Nonces are single-use.
func (a *NoopAttestor) SubmitQuote(_ context.Context, vmID string, req QuoteRequest) (QuoteResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	issued, ok := a.nonces[vmID][req.Nonce]
	if ok {
		delete(a.nonces[vmID], req.Nonce)
	}
	a.sweepLocked(now)
	if !ok || now.Sub(issued) > NonceTTL {
		return QuoteResult{Message: "unknown or expired nonce"}, nil
	}
	return QuoteResult{Verified: true, Message: "nonce matched; quote not verified (noop attestor)"}, nil
}

func (a *NoopAttestor) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
