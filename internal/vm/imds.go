package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"time"

	"github.com/giantswarm/vm-manager/internal/attest"
	"github.com/giantswarm/vm-manager/internal/imds"
)

const imdsHeaderTimeout = 10 * time.Second

// LookupByIP implements imds.Resolver: the guest behind a lease address.
func (s *Service) LookupByIP(_ context.Context, ip netip.Addr) (imds.Instance, bool) {
	addr := ip.Unmap().String()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.vms {
		if e.rec.IP == addr && e.rec.State != StateDeleting {
			return instanceOf(e), true
		}
	}
	return imds.Instance{}, false
}

func instanceOf(e *entry) imds.Instance {
	rec := e.rec.clone()
	return imds.Instance{
		ID:                rec.ID,
		Name:              rec.Name,
		Hostname:          rec.Hostname,
		Region:            rec.Region,
		Zone:              rec.Zone,
		SSHAuthorizedKeys: append([]string{rec.SSHPublicKey}, rec.SSHAuthorizedKeys...),
		KubernetesVersion: rec.KubernetesVersion,
		Metadata:          rec.Metadata,
		UserData:          e.userData,
		UserDataReleased:  rec.Attestation.UserDataReleased,
	}
}

// StoreReport implements imds.ReportSink: the last upload is kept in memory
// and as report.json, then handed to the metrics registry. A report the
// registry cannot parse is still stored, so GET /vms/{id}/report shows what
// the guest sent.
func (s *Service) StoreReport(ctx context.Context, vmID string, report json.RawMessage) error {
	s.mu.Lock()
	e, err := s.lookup(vmID)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	e.report = append([]byte(nil), report...)
	err = writeAtomic(e.rec.Paths.Report, e.report, 0o600)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.opts.Metrics.StoreReport(ctx, vmID, report); err != nil {
		s.log.Warn("guest report not exported as metrics", "id", vmID, "err", err)
	}
	return nil
}

// PolicyFor implements attest.PolicyProvider: the policy of the image the
// VM was created from. Errors wrap attest.ErrNoPolicy when the image or its
// policy is missing or invalid, apierr.ErrNotFound for an unknown VM.
func (s *Service) PolicyFor(_ context.Context, vmID string) (attest.Policy, error) {
	v, err := s.Get(vmID)
	if err != nil {
		return attest.Policy{}, err
	}
	img, err := s.opts.Images.Get(v.Image)
	if err != nil {
		return attest.Policy{}, fmt.Errorf("%w: image %s of vm %s: %v", attest.ErrNoPolicy, v.Image, vmID, err)
	}
	if img.Policy == nil {
		return attest.Policy{}, fmt.Errorf("%w: image %s has no policy.json", attest.ErrNoPolicy, img.Ref())
	}
	p, err := attest.ParsePolicy(img.Policy)
	if err != nil {
		return attest.Policy{}, fmt.Errorf("%w: image %s: %v", attest.ErrNoPolicy, img.Ref(), err)
	}
	return p, nil
}

// ResultSource is implemented by attestors that keep more than the verdict
// of a quote (attest.Verifier); the record copies the details.
type ResultSource interface {
	Result(vmID string, stage imds.Stage) (attest.Result, bool)
}

// Forgetter is implemented by attestors that hold per-VM state
// (attest.Verifier: the pinned key, verdicts, nonces); Delete calls it.
type Forgetter interface {
	Forget(vmID string)
}

// recordingAttestor wraps the configured Attestor and writes its verdicts
// into the VM record: the first nonce moves a booting VM to attesting, a
// verified initrd quote releases user-data (imds.ReleasesUserData). The
// QuoteResult passes through; the IMDS handler fills in UserDataReleased.
type recordingAttestor struct {
	s     *Service
	inner imds.Attestor
}

func (a recordingAttestor) Nonce(ctx context.Context, vmID string) (string, error) {
	nonce, err := a.inner.Nonce(ctx, vmID)
	if err != nil {
		return "", err
	}
	a.s.onNonce(vmID)
	return nonce, nil
}

func (a recordingAttestor) SubmitQuote(ctx context.Context, vmID string, req imds.QuoteRequest) (imds.QuoteResult, error) {
	res, err := a.inner.SubmitQuote(ctx, vmID, req)
	if err != nil {
		return res, err
	}
	q := &Quote{Verified: res.Verified, Message: res.Message, At: a.s.clock.Now()}
	if src, ok := a.inner.(ResultSource); ok {
		if r, ok := src.Result(vmID, req.Stage); ok {
			q.AKFingerprint, q.PCRs, q.Learned = r.AKFingerprint, r.PCRs, r.Learned
		}
	}
	a.s.recordQuote(vmID, req.Stage, q)
	return res, nil
}

func (s *Service) onNonce(vmID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookup(vmID)
	if err != nil {
		return
	}
	if e.rec.Attestation.NonceIssuedAt == nil {
		e.rec.Attestation.NonceIssuedAt = s.now()
	}
	if e.rec.Attestation.Required && e.rec.State == StateBooting {
		e.rec.State = StateAttesting
	}
	s.save(e)
	s.broadcastLocked()
}

// recordQuote stores the verdict and releases user-data when it applies.
func (s *Service) recordQuote(vmID string, stage imds.Stage, q *Quote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookup(vmID)
	if err != nil {
		return
	}
	switch stage {
	case imds.StageInitrd:
		e.rec.Attestation.Initrd = q
	case imds.StageReady:
		e.rec.Attestation.Ready = q
	}
	if imds.ReleasesUserData(stage, q.Verified) {
		e.rec.Attestation.UserDataReleased = true
	}
	s.save(e)
	s.broadcastLocked()
	s.log.Info("attestation quote", "id", vmID, "stage", stage, "verified", q.Verified, "ak", q.AKFingerprint, "userDataReleased", e.rec.Attestation.UserDataReleased)
}

// IMDSDeps is the imds.Handler wiring of this service: resolver, recording
// attestor, report sink and the sysupdate tree of the image directory.
func (s *Service) IMDSDeps() imds.Deps {
	return imds.Deps{
		Resolver:  s,
		Attestor:  recordingAttestor{s: s, inner: s.opts.Attestor},
		Reports:   s,
		Artifacts: imds.FSArtifacts{FS: os.DirFS(s.opts.Images.SysupdateDir())},
		Log:       s.log,
	}
}

// ServeIMDS serves the metadata service inside the named network. It is
// idempotent and must run before a guest on that network boots; Load and
// CreateNetwork call it.
func (s *Service) ServeIMDS(_ context.Context, name string) error {
	s.mu.Lock()
	_, running := s.imds[name]
	s.mu.Unlock()
	if running {
		return nil
	}
	nw, err := s.opts.Networks.Get(name)
	if err != nil {
		return err
	}
	ln, err := nw.ListenIMDS()
	if err != nil {
		return fmt.Errorf("listen imds on %s: %w", name, err)
	}
	srv := &http.Server{Handler: imds.Handler(s.IMDSDeps()), ReadHeaderTimeout: imdsHeaderTimeout}

	s.mu.Lock()
	if _, running := s.imds[name]; running {
		s.mu.Unlock()
		return ln.Close()
	}
	s.imds[name] = &imdsServer{srv: srv, ln: ln}
	s.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Warn("imds server stopped", "network", name, "err", err)
		}
	}()
	s.log.Info("imds serving", "network", name)
	return nil
}

func (i *imdsServer) close(ctx context.Context) error {
	sctx, cancel := context.WithTimeout(ctx, imdsHeaderTimeout)
	defer cancel()
	if err := i.srv.Shutdown(sctx); err != nil {
		_ = i.srv.Close()
		return err
	}
	return nil
}
