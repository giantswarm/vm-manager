// Package storage hands out the disk volumes VMs boot from. Every VM gets a
// target volume of the size the API requested (GiB there, bytes here); the
// runtime attaches it to QEMU as virtio-blk with serial=target and the
// installer image writes the OS onto it (docs/design.md, "Boot flow").
//
// # Providers
//
// SystemdProvider talks io.systemd.StorageProvider (see
// testdata/io.systemd.StorageProvider.varlink for the introspected
// interface) over internal/varlink to systemd-storage-fs, which keeps
// volumes as NAME.volume entries below /var/lib/storage (system) or
// $XDG_STATE_HOME/storage (user). Acquire creates volumes on demand from
// the sparse-file template; the interface has no release or delete call:
// a volume is released by closing its file descriptor and deleted by
// removing its NAME.volume entry, which is what Release and Delete do.
//
// FileProvider is the fallback for hosts without a storage provider
// (GitHub Actions runners on systemd 255, containers): sparse raw files
// with the same NAME.volume layout under a directory of vm-manager's own.
//
// Detect picks the first provider that answers: as root the system fs
// socket, otherwise the user socket first because only there can the
// process also delete what it created; then FileProvider.
//
// # File descriptor versus path
//
// The systemd provider returns an open file descriptor, not a path. The
// runtime needs a path for QEMU, whose block layer wants to open the
// backing file itself (cache mode, OFD locking, reopen on snapshot or
// migration). Volume therefore carries both: Path is the provider's
// NAME.volume entry (or, for device nodes, the descriptor's /proc/self/fd
// target), verified with fstat/stat to be the inode the descriptor refers
// to, and File keeps the descriptor open until Close so the provider's
// lease outlives any concurrent removal of the entry. /proc/self/fd alone
// is not enough: systemd-storage-fs creates volumes with O_TMPFILE and the
// link then reads "#ino (deleted)". The runtime passes Path to QEMU; File
// is available should it prefer to inherit the descriptor through
// ExtraFiles and /dev/fdset instead.
//
// # Errors
//
// Provider errors wrap the internal/apierr sentinels: ErrNotFound for a
// missing volume, ErrConflict when a volume exists or is read-only,
// ErrInvalid for names, sizes and templates the caller can fix, and
// ErrUnsupported for volume kinds a VM cannot boot from. The Varlink error
// stays in the chain for logs.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/host"
	"github.com/giantswarm/vm-manager/internal/varlink"
)

// VolumeSuffix is the file name suffix systemd-storage-fs uses for volume
// entries; FileProvider mirrors it so both layouts read alike.
const VolumeSuffix = ".volume"

// Kind is the inode type of a volume, following the provider's naming.
type Kind int

const (
	// KindFile is a regular file ("reg") used as a raw disk image.
	KindFile Kind = iota
	// KindBlock is a block device node ("blk").
	KindBlock
)

func (k Kind) String() string {
	if k == KindBlock {
		return "blk"
	}
	return "reg"
}

// CreateMode says whether Acquire opens an existing volume, creates a new
// one, or accepts either. The zero value never creates.
type CreateMode int

const (
	// CreateOpen opens an existing volume and fails with ErrNotFound otherwise.
	CreateOpen CreateMode = iota
	// CreateNew creates the volume and fails with ErrConflict if it exists.
	CreateNew
	// CreateAny opens the volume if it exists and creates it otherwise.
	CreateAny
)

func (m CreateMode) String() string {
	switch m {
	case CreateNew:
		return "new"
	case CreateAny:
		return "any"
	default:
		return "open"
	}
}

// Templates of systemd-storage-fs that FileProvider also understands.
const (
	// TemplateSparseFile is the default: a sparsely allocated regular file.
	TemplateSparseFile = "sparse-file"
	// TemplateAllocatedFile is a fully allocated regular file.
	TemplateAllocatedFile = "allocated-file"
)

// AcquireSpec describes the volume to acquire.
type AcquireSpec struct {
	// Name identifies the volume; a single path component, one per VM.
	Name string
	// SizeBytes is the size of a volume that gets created; ignored when an
	// existing volume is opened.
	SizeBytes int64
	// Create selects open, new or any.
	Create CreateMode
	// ReadOnly opens the volume read-only; otherwise it must be writable.
	ReadOnly bool
	// Template names the provider template for a created volume; empty
	// means the provider default, a sparse regular file.
	Template string
}

// Volume is an acquired volume. Close releases it; the entry stays until
// Provider.Delete removes it.
type Volume struct {
	// Name is the volume name.
	Name string
	// Path is the host path QEMU opens: the regular file or device node.
	Path string
	// SizeBytes is the current size of the file or device.
	SizeBytes int64
	// Kind is the inode type.
	Kind Kind
	// ReadOnly reports whether the volume was acquired read-only.
	ReadOnly bool

	file *os.File
}

// File is the open descriptor held for the volume's lifetime, positioned
// at offset 0. It is closed by Close; do not close it directly.
func (v *Volume) File() *os.File { return v.file }

// Close releases the volume. It is safe to call more than once.
func (v *Volume) Close() error {
	if v.file == nil {
		return nil
	}
	err := v.file.Close()
	v.file = nil
	return err
}

// Info describes a volume a provider lists.
type Info struct {
	Name      string
	Kind      Kind
	SizeBytes int64
	ReadOnly  bool
}

// Provider hands out volumes.
type Provider interface {
	// Name identifies the implementation ("systemd" or "file") for logs
	// and host reports.
	Name() string
	// Acquire opens or creates a volume.
	Acquire(ctx context.Context, spec AcquireSpec) (*Volume, error)
	// Release gives a volume back; the entry is kept.
	Release(ctx context.Context, v *Volume) error
	// Delete removes a volume's entry. The volume must not be in use.
	Delete(ctx context.Context, name string) error
	// List returns the volumes whose names match the shell glob (all when
	// empty), regular files and block devices only.
	List(ctx context.Context, glob string) ([]Info, error)
}

// Detect returns the first usable provider: a systemd storage provider
// whose socket answers GetInfo, else a FileProvider below fallbackDir.
func Detect(ctx context.Context, logger *slog.Logger, fallbackDir string) Provider {
	for _, c := range systemdCandidates() {
		if _, err := os.Stat(c.socket); err != nil {
			continue
		}
		info, err := probe(ctx, c.socket)
		if err != nil {
			logger.Warn("storage provider socket did not answer", "socket", c.socket, "err", err)
			continue
		}
		logger.Info("using systemd storage provider", "socket", c.socket, "volumes", c.volumeDir, "product", info.Product, "version", info.Version)
		return NewSystemdProvider(c.socket, c.volumeDir)
	}
	logger.Info("no systemd storage provider, using plain files", "dir", fallbackDir)
	return NewFileProvider(fallbackDir)
}

// probeTimeout bounds Detect's handshake with one provider socket so a
// listening but unresponsive provider cannot stall startup.
const probeTimeout = 3 * time.Second

func probe(ctx context.Context, socket string) (*varlink.ServiceInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := varlink.Dial(ctx, socket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	info, err := conn.Info(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(info.Interfaces, interfaceName) {
		return nil, fmt.Errorf("%s does not implement %s", info.Product, interfaceName)
	}
	return info, nil
}

type candidate struct{ socket, volumeDir string }

// systemdCandidates orders the fs provider sockets so the first one is the
// one whose volume directory the process can also delete from.
func systemdCandidates() []candidate {
	system := candidate{SystemSocket, SystemVolumeDir}
	user := candidate{UserSocket(), UserVolumeDir()}
	if os.Geteuid() == 0 {
		return []candidate{system, user}
	}
	return []candidate{user, system}
}

// validateName accepts a single path component that is not a dot entry.
func validateName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return fmt.Errorf("%w: volume name %q", apierr.ErrInvalid, name)
	case strings.ContainsAny(name, "/\x00"):
		return fmt.Errorf("%w: volume name %q must be a single path component", apierr.ErrInvalid, name)
	case len(name)+len(VolumeSuffix) > 255:
		return fmt.Errorf("%w: volume name %q too long", apierr.ErrInvalid, name)
	}
	return nil
}

// newVolume builds a Volume around an open descriptor, taking ownership of
// f and closing it on error.
func newVolume(name, path string, f *os.File, readOnly bool) (*Volume, error) {
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat volume %s: %w", name, err)
	}
	v := &Volume{Name: name, Path: path, ReadOnly: readOnly, file: f}
	switch {
	case fi.Mode().IsRegular():
		v.Kind = KindFile
		v.SizeBytes = fi.Size()
	case fi.Mode()&os.ModeDevice != 0 && fi.Mode()&os.ModeCharDevice == 0:
		v.Kind = KindBlock
		if v.SizeBytes, err = blockSize(f); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("size of block volume %s: %w", name, err)
		}
	default:
		_ = f.Close()
		return nil, fmt.Errorf("%w: volume %s is a %s, not a regular file or block device", apierr.ErrUnsupported, name, fi.Mode().Type())
	}
	return v, nil
}

// blockSize measures a block device by seeking to its end and back.
func blockSize(f *os.File) (int64, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return size, nil
}

// volumePath is the entry for name below dir.
func volumePath(dir, name string) string {
	return filepath.Join(dir, name+VolumeSuffix)
}

// removeEntry deletes a volume entry, mapping the usual failures.
func removeEntry(path, name string) error {
	err := os.Remove(path)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: volume %s", apierr.ErrNotFound, name)
	case errors.Is(err, os.ErrExist): // EEXIST or ENOTEMPTY from rmdir(2)
		return fmt.Errorf("%w: volume %s is a directory volume, which vm-manager does not manage", apierr.ErrUnsupported, name)
	default:
		return fmt.Errorf("delete volume %s: %w", name, err)
	}
}

// Sockets and volume directories of systemd-storage-fs.
const (
	// SystemSocket is the system fs provider's Varlink socket.
	SystemSocket = host.StorageProviderDir + "/" + host.StorageProviderFS
	// SystemVolumeDir is where the system fs provider keeps volumes.
	SystemVolumeDir = "/var/lib/storage"
)

// UserSocket is the user fs provider's socket below $XDG_RUNTIME_DIR.
func UserSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(dir, "systemd", "io.systemd.StorageProvider", host.StorageProviderFS)
}

// UserVolumeDir is where the user fs provider keeps volumes, below
// $XDG_STATE_HOME (default ~/.local/state).
func UserVolumeDir() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/"
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "storage")
}
