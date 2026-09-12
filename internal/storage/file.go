package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

// FileProvider keeps volumes as raw files named NAME.volume below Dir. It
// is the fallback where no systemd storage provider runs and mirrors the
// fs provider's semantics: sparse files by default, allocated on request,
// created with the size given and opened as they are afterwards.
type FileProvider struct {
	dir string
}

// NewFileProvider returns a provider storing volumes below dir, which is
// created on first use.
func NewFileProvider(dir string) *FileProvider {
	return &FileProvider{dir: dir}
}

// Name implements Provider.
func (p *FileProvider) Name() string { return "file" }

// VolumeDir is the directory volume files live in.
func (p *FileProvider) VolumeDir() string { return p.dir }

// Acquire implements Provider.
func (p *FileProvider) Acquire(_ context.Context, spec AcquireSpec) (*Volume, error) {
	if err := validateName(spec.Name); err != nil {
		return nil, err
	}
	if spec.Template != "" && spec.Template != TemplateSparseFile && spec.Template != TemplateAllocatedFile {
		return nil, fmt.Errorf("%w: volume %s: template %q not supported by the file provider", apierr.ErrInvalid, spec.Name, spec.Template)
	}
	if spec.Create != CreateOpen && spec.SizeBytes <= 0 {
		return nil, fmt.Errorf("%w: volume %s: a size is required to create a volume", apierr.ErrInvalid, spec.Name)
	}
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		return nil, fmt.Errorf("volume directory: %w", err)
	}

	path := volumePath(p.dir, spec.Name)
	f, created, err := p.open(path, spec)
	if err != nil {
		return nil, err
	}
	if created {
		if err := allocate(f, spec); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("create volume %s: %w", spec.Name, err)
		}
		if spec.ReadOnly {
			_ = f.Close()
			if f, err = os.OpenFile(path, os.O_RDONLY, 0); err != nil { // #nosec G304 -- path is dir + validated name
				return nil, fmt.Errorf("reopen volume %s: %w", spec.Name, err)
			}
		}
	}
	return newVolume(spec.Name, path, f, spec.ReadOnly)
}

// open opens or creates the volume file per spec.Create and reports
// whether it created it.
func (p *FileProvider) open(path string, spec AcquireSpec) (*os.File, bool, error) {
	flags := os.O_RDWR
	if spec.ReadOnly && spec.Create == CreateOpen {
		flags = os.O_RDONLY
	}
	if spec.Create != CreateOpen {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path is dir + validated name
		switch {
		case err == nil:
			return f, true, nil
		case !errors.Is(err, os.ErrExist):
			return nil, false, fmt.Errorf("create volume %s: %w", spec.Name, err)
		case spec.Create == CreateNew:
			return nil, false, fmt.Errorf("%w: volume %s exists", apierr.ErrConflict, spec.Name)
		}
		if spec.ReadOnly {
			flags = os.O_RDONLY
		}
	}
	f, err := os.OpenFile(path, flags, 0) // #nosec G304 -- path is dir + validated name
	switch {
	case err == nil:
		return f, false, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, false, fmt.Errorf("%w: volume %s", apierr.ErrNotFound, spec.Name)
	case errors.Is(err, os.ErrPermission) && !spec.ReadOnly:
		return nil, false, fmt.Errorf("%w: volume %s is read-only", apierr.ErrConflict, spec.Name)
	default:
		return nil, false, fmt.Errorf("open volume %s: %w", spec.Name, err)
	}
}

// allocate sizes a freshly created file: sparse via truncate, or fully
// allocated with fallocate(2) for the allocated-file template.
func allocate(f *os.File, spec AcquireSpec) error {
	if err := f.Truncate(spec.SizeBytes); err != nil {
		return err
	}
	if spec.Template == TemplateAllocatedFile {
		return unix.Fallocate(int(f.Fd()), 0, 0, spec.SizeBytes)
	}
	return nil
}

// Release implements Provider.
func (p *FileProvider) Release(_ context.Context, v *Volume) error {
	return v.Close()
}

// Delete implements Provider.
func (p *FileProvider) Delete(_ context.Context, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	return removeEntry(volumePath(p.dir, name), name)
}

// List implements Provider.
func (p *FileProvider) List(_ context.Context, glob string) ([]Info, error) {
	if glob == "" {
		glob = "*"
	}
	matches, err := filepath.Glob(volumePath(p.dir, glob))
	if err != nil {
		return nil, fmt.Errorf("%w: glob %q: %w", apierr.ErrInvalid, glob, err)
	}
	out := make([]Info, 0, len(matches))
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		name := filepath.Base(m)
		out = append(out, Info{
			Name:      name[:len(name)-len(VolumeSuffix)],
			Kind:      KindFile,
			SizeBytes: fi.Size(),
			ReadOnly:  fi.Mode().Perm()&0o200 == 0,
		})
	}
	return out, nil
}
