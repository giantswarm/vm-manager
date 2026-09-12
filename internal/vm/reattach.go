package vm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/runtime/proc"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/tpm"
)

// reattach picks the processes of a VM recorded live back up after a
// vm-manager restart, in the order a start would have created them: the
// target volume is reopened, the network lease looked up (QEMU reconnects
// its netdev by itself once the network's socket is back), swtpm and QEMU
// are attached through the handles in the record, the notify subscription
// is renewed for an installed boot, and the supervisor resumes, so a later
// exit is handled exactly as it would have been before the restart. A stop
// that was in flight is resumed. The record's state is left as it was.
//
// Load calls it before any request is served and before the entry is in
// use, so the record is read without the lock; the caller settles a
// failure.
func (s *Service) reattach(ctx context.Context, e *entry) (err error) {
	rec := &e.rec
	if rec.Processes == nil {
		return errors.New("no process handles recorded")
	}
	if err := s.openVolume(ctx, e); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			s.closeVolume(ctx, e)
		}
	}()
	nw, err := s.opts.Networks.Get(rec.Network)
	if err != nil {
		return fmt.Errorf("network %q: %w", rec.Network, err)
	}
	att, err := nw.Attach(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("attach network: %w", err)
	}

	t, err := s.opts.TPM.Attach(ctx, tpmConfig(rec), rec.Processes.TPM)
	if err != nil {
		// QEMU runs on without its TPM; the guest sees a device that no
		// longer answers. Track the VM regardless.
		s.log.Warn("swtpm not reattached", "id", rec.ID, "err", err)
		t = missingTPM{socket: filepath.Join(rec.Paths.TPMState, tpm.DefaultSocketName)}
	}
	var img images.Image
	if rec.Phase == qemu.PhaseInstall {
		img, _ = s.opts.Images.Get(rec.Image)
	}
	spec := s.qemuSpec(rec, rec.Phase, img, att.SocketPath, att.MAC, t.SocketPath())
	inst, err := s.opts.Runtime.Attach(ctx, spec, rec.Processes.QEMU)
	if err != nil {
		s.stopTPM(rec.ID, t)
		return err
	}

	p := &process{phase: rec.Phase, tpm: t, inst: inst, wait: inst.Wait(), done: make(chan struct{})}
	if rec.Phase == qemu.PhaseBoot {
		p.notify = s.opts.Notify.Subscribe(rec.CID)
	}
	s.mu.Lock()
	e.proc = p
	s.mu.Unlock()
	s.supervise(e, p)
	if rec.State == StateStopping {
		go s.stopProcess(s.ctx, rec.ID, p)
	}
	return nil
}

// missingTPM stands in for a swtpm that was gone at reattach: there is
// nothing to stop, and no handle to persist.
type missingTPM struct{ socket string }

func (m missingTPM) SocketPath() string       { return m.socket }
func (missingTPM) Stop(context.Context) error { return nil }
func (missingTPM) Handle() proc.Handle        { return proc.Handle{} }
