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
// and as report.json for the metrics package.
func (s *Service) StoreReport(_ context.Context, vmID string, report json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookup(vmID)
	if err != nil {
		return err
	}
	e.report = append([]byte(nil), report...)
	return writeAtomic(e.rec.Paths.Report, e.report, 0o600)
}

// recordingAttestor wraps the configured Attestor and writes its verdicts
// into the VM record: the first nonce moves a booting VM to attesting, a
// verified initrd quote releases user-data (imds.ReleasesUserData).
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
	res.UserDataReleased = a.s.recordQuote(vmID, req.Stage, res)
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

// recordQuote stores the verdict and reports whether user-data is released.
func (s *Service) recordQuote(vmID string, stage imds.Stage, res imds.QuoteResult) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookup(vmID)
	if err != nil {
		return false
	}
	q := &Quote{Verified: res.Verified, Message: res.Message, At: s.clock.Now()}
	switch stage {
	case imds.StageInitrd:
		e.rec.Attestation.Initrd = q
	case imds.StageReady:
		e.rec.Attestation.Ready = q
	}
	if imds.ReleasesUserData(stage, res.Verified) {
		e.rec.Attestation.UserDataReleased = true
	}
	s.save(e)
	s.broadcastLocked()
	s.log.Info("attestation quote", "id", vmID, "stage", stage, "verified", res.Verified, "userDataReleased", e.rec.Attestation.UserDataReleased)
	return e.rec.Attestation.UserDataReleased
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
