package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/varlink"
)

// Names from testdata/io.systemd.StorageProvider.varlink.
const (
	interfaceName     = "io.systemd.StorageProvider"
	methodAcquire     = interfaceName + ".Acquire"
	methodListVolumes = interfaceName + ".ListVolumes"

	errNoSuchVolume       = interfaceName + ".NoSuchVolume"
	errVolumeExists       = interfaceName + ".VolumeExists"
	errNoSuchTemplate     = interfaceName + ".NoSuchTemplate"
	errTypeNotSupported   = interfaceName + ".TypeNotSupported"
	errWrongType          = interfaceName + ".WrongType"
	errCreateNotSupported = interfaceName + ".CreateNotSupported"
	errCreateSizeRequired = interfaceName + ".CreateSizeRequired"
	errReadOnlyVolume     = interfaceName + ".ReadOnlyVolume"
	errBadTemplate        = interfaceName + ".BadTemplate"
	errInvalidParameter   = "org.varlink.service.InvalidParameter"

	typeReg = "reg"
	typeBlk = "blk"
)

// acquireParams is the Acquire request. Optional fields are pointers or
// omitempty so unset ones are not sent.
type acquireParams struct {
	Name            string `json:"name"`
	CreateMode      string `json:"createMode"`
	Template        string `json:"template,omitempty"`
	ReadOnly        bool   `json:"readOnly"`
	RequestAs       string `json:"requestAs,omitempty"`
	CreateSizeBytes int64  `json:"createSizeBytes,omitempty"`
}

type acquireReply struct {
	FileDescriptorIndex int    `json:"fileDescriptorIndex"`
	Type                string `json:"type"`
	ReadOnly            bool   `json:"readOnly"`
}

type listVolumesReply struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	ReadOnly  bool   `json:"readOnly"`
	SizeBytes int64  `json:"sizeBytes"`
}

// SystemdProvider acquires volumes from a systemd storage provider socket
// and manages their entries in the provider's volume directory.
type SystemdProvider struct {
	socket    string
	volumeDir string
}

// NewSystemdProvider returns a provider for the given socket; volumeDir is
// the provider's storage directory, where Delete removes entries.
func NewSystemdProvider(socket, volumeDir string) *SystemdProvider {
	return &SystemdProvider{socket: socket, volumeDir: volumeDir}
}

// Name implements Provider.
func (p *SystemdProvider) Name() string { return "systemd" }

// Socket is the Varlink socket in use.
func (p *SystemdProvider) Socket() string { return p.socket }

// VolumeDir is the directory volume entries live in.
func (p *SystemdProvider) VolumeDir() string { return p.volumeDir }

// Acquire implements Provider. Created volumes are always requested as
// regular files: without requestAs the system provider defaults to a
// directory volume (its "subvolume" template), which no VM can boot from.
func (p *SystemdProvider) Acquire(ctx context.Context, spec AcquireSpec) (*Volume, error) {
	if err := validateName(spec.Name); err != nil {
		return nil, err
	}
	if spec.SizeBytes < 0 {
		return nil, fmt.Errorf("%w: volume %s: negative size", apierr.ErrInvalid, spec.Name)
	}
	params := acquireParams{
		Name:            spec.Name,
		CreateMode:      spec.Create.String(),
		Template:        spec.Template,
		ReadOnly:        spec.ReadOnly,
		CreateSizeBytes: spec.SizeBytes,
	}
	if spec.Create != CreateOpen {
		params.RequestAs = typeReg
	}

	conn, err := varlink.Dial(ctx, p.socket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	var rep acquireReply
	files, err := conn.CallWithFiles(ctx, methodAcquire, params, &rep)
	if err != nil {
		return nil, mapError(spec.Name, err)
	}
	f, err := pickFile(files, rep.FileDescriptorIndex)
	if err != nil {
		return nil, fmt.Errorf("acquire volume %s: %w", spec.Name, err)
	}
	if rep.Type != typeReg && rep.Type != typeBlk {
		_ = f.Close()
		return nil, fmt.Errorf("%w: volume %s is of type %q, not a regular file or block device", apierr.ErrUnsupported, spec.Name, rep.Type)
	}
	path, err := p.resolvePath(spec.Name, f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire volume %s: %w", spec.Name, err)
	}
	return newVolume(spec.Name, path, f, rep.ReadOnly)
}

// Release implements Provider by closing the descriptor; the provider
// keeps no server-side state per acquisition.
func (p *SystemdProvider) Release(_ context.Context, v *Volume) error {
	return v.Close()
}

// Delete implements Provider by removing NAME.volume from the volume
// directory, the way systemd-storage-fs(8) documents administrators
// manage volumes.
func (p *SystemdProvider) Delete(_ context.Context, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	return removeEntry(volumePath(p.volumeDir, name), name)
}

// List implements Provider via ListVolumes, a "more" stream.
func (p *SystemdProvider) List(ctx context.Context, glob string) ([]Info, error) {
	conn, err := varlink.Dial(ctx, p.socket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	params := map[string]string{}
	if glob != "" {
		params["matchName"] = glob
	}
	var out []Info
	err = conn.CallMore(ctx, methodListVolumes, params, func(raw json.RawMessage) error {
		var r listVolumesReply
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		switch r.Type {
		case typeReg:
			out = append(out, Info{Name: r.Name, Kind: KindFile, SizeBytes: r.SizeBytes, ReadOnly: r.ReadOnly})
		case typeBlk:
			out = append(out, Info{Name: r.Name, Kind: KindBlock, SizeBytes: r.SizeBytes, ReadOnly: r.ReadOnly})
		}
		return nil
	})
	if err != nil {
		return nil, mapError(glob, err)
	}
	return out, nil
}

// pickFile takes the file at index and closes the rest.
func pickFile(files []*os.File, index int) (*os.File, error) {
	if index < 0 || index >= len(files) {
		for _, f := range files {
			_ = f.Close()
		}
		return nil, fmt.Errorf("reply refers to file descriptor %d but %d were received", index, len(files))
	}
	for i, f := range files {
		if i != index {
			_ = f.Close()
		}
	}
	return files[index], nil
}

// resolvePath finds the path QEMU opens for the acquired descriptor and
// verifies with fstat/stat that it names the descriptor's inode.
// systemd-storage-fs creates volumes with O_TMPFILE and links them into
// place, so the descriptor's /proc/self/fd link reads "#ino (deleted)"
// even though the entry exists; the documented NAME.volume entry is
// therefore checked first and the link second, which covers block device
// nodes symlinked into the volume directory.
func (p *SystemdProvider) resolvePath(name string, f *os.File) (string, error) {
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat volume descriptor: %w", err)
	}
	candidates := []string{volumePath(p.volumeDir, name)}
	link, err := os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(f.Fd()), 10))
	if err == nil && !strings.HasSuffix(link, " (deleted)") {
		candidates = append(candidates, link)
	}
	for _, c := range candidates {
		if pi, err := os.Stat(c); err == nil && os.SameFile(fi, pi) {
			return c, nil
		}
	}
	return "", fmt.Errorf("no path names the inode behind the volume descriptor (checked %s)", strings.Join(candidates, ", "))
}

// mapError wraps Varlink error replies in the apierr sentinels.
func mapError(name string, err error) error {
	var verr *varlink.Error
	if !errors.As(err, &verr) {
		return err
	}
	var sentinel error
	switch verr.Name {
	case errNoSuchVolume:
		sentinel = apierr.ErrNotFound
	case errVolumeExists, errWrongType, errReadOnlyVolume:
		sentinel = apierr.ErrConflict
	case errNoSuchTemplate, errBadTemplate, errCreateSizeRequired, errInvalidParameter:
		sentinel = apierr.ErrInvalid
	case errTypeNotSupported, errCreateNotSupported:
		sentinel = apierr.ErrUnsupported
	default:
		return fmt.Errorf("volume %s: %w", name, err)
	}
	return fmt.Errorf("%w: volume %s: %w", sentinel, name, err)
}
