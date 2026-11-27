package layersign

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/containerd/go-dmverity/pkg/verity"
	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
)

// The device a node opens for a tar layer is the EROFS tar index followed by
// the whole uncompressed tar (the diff_id bytes), zero-padded to blockSize;
// dm-verity hashes it in blocks of blockSize with format 1, SHA-256 and a salt
// of 32 zero bytes.
const (
	blockSize = 512
	saltSize  = 32
	hashName  = "sha256"
)

// mkfsErofs runs erofs-utils' mkfs.erofs to build tar indexes.
type mkfsErofs struct {
	path    string
	version string
}

var mkfsVersionRE = regexp.MustCompile(`erofs-utils\)\s+(\S+)`)

// newMkfsErofs finds mkfs.erofs (name on PATH, or a path) and checks that it
// can build a reproducible tar index (--tar=i, --mkfs-time).
func newMkfsErofs(ctx context.Context, name string) (*mkfsErofs, error) {
	if name == "" {
		name = "mkfs.erofs"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("%s not found: install erofs-utils (the vm-manager image carries it): %w", name, err)
	}
	out, _ := exec.CommandContext(ctx, path, "--version").CombinedOutput() //nolint:gosec // G204: the mkfs.erofs the operator chose
	m := mkfsVersionRE.FindSubmatch(out)
	if m == nil {
		return nil, fmt.Errorf("%s --version: no erofs-utils version in %q", path, strings.TrimSpace(string(out)))
	}
	help, _ := exec.CommandContext(ctx, path, "--help").CombinedOutput() //nolint:gosec // G204: the mkfs.erofs the operator chose
	for _, flag := range []string{"--tar=", "--aufs", "--mkfs-time"} {
		if !bytes.Contains(help, []byte(flag)) {
			return nil, fmt.Errorf("%s (erofs-utils %s) is too old: it lacks %s, which a reproducible tar index needs; install a newer erofs-utils", path, m[1], flag)
		}
	}
	return &mkfsErofs{path: path, version: string(m[1])}, nil
}

// tarIndexUUID is the file-system UUID of a layer's index, derived from the
// manifest's (compressed) layer digest as containerd's EROFS differ does.
func tarIndexUUID(layer digest.Digest) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("erofs:blobs/"+layer.String())).String()
}

// verityUUID is the dm-verity superblock UUID of a layer's hash tree: fixed
// per layer digest, so a re-sign gives the same tree blob.
func verityUUID(layer digest.Digest) [16]byte {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("dm-verity:blobs/"+layer.String()))
}

// index writes the tar index of the tar file tarPath to out.
func (m *mkfsErofs) index(ctx context.Context, tarPath, out string, layer digest.Digest) error {
	tar, err := os.Open(tarPath) //nolint:gosec // G304: a file of the work directory
	if err != nil {
		return err
	}
	defer func() { _ = tar.Close() }()
	//nolint:gosec // G204: the mkfs.erofs the operator chose, fixed flags
	cmd := exec.CommandContext(ctx, m.path, "--tar=i", "--aufs", "--quiet", "-T0", "--mkfs-time", "-U", tarIndexUUID(layer), out)
	cmd.Stdin = tar
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs.erofs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// writeDevice writes index ‖ tar, zero-padded to blockSize, to out and
// returns its size.
func writeDevice(indexPath, tarPath, out string) (int64, error) {
	f, err := os.Create(out) //nolint:gosec // G304: a file of the work directory
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	var size int64
	for _, p := range []string{indexPath, tarPath} {
		in, err := os.Open(p) //nolint:gosec // G304: a file of the work directory
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(f, in)
		_ = in.Close()
		if err != nil {
			return 0, err
		}
		size += n
	}
	if pad := (blockSize - size%blockSize) % blockSize; pad > 0 {
		if _, err := f.Write(make([]byte, pad)); err != nil {
			return 0, err
		}
		size += pad
	}
	return size, f.Close()
}

func verityParams(deviceSize int64) verity.Params {
	p := verity.DefaultParams()
	p.DataBlockSize = blockSize
	p.HashBlockSize = blockSize
	p.DataBlocks = uint64(deviceSize / blockSize) //nolint:gosec // G115: a file size is not negative
	p.Salt = make([]byte, saltSize)
	p.SaltSize = saltSize
	// The hash file holds the superblock, then the tree at the next hash
	// block: the bytes a node appends to the device.
	p.HashAreaOffset = blockSize
	return p
}

// writeTree writes the dm-verity superblock and hash tree of the device to
// out and returns the root hash, lowercase hex.
func writeTree(devicePath string, deviceSize int64, out string, layer digest.Digest) (string, error) {
	p := verityParams(deviceSize)
	p.UUID = verityUUID(layer)
	root, err := verity.Create(&p, devicePath, out)
	if err != nil {
		return "", fmt.Errorf("dm-verity: %w", err)
	}
	return hex.EncodeToString(root), nil
}

// checkTree checks that the published superblock carries the parameters a
// layer signature is made with for a device of deviceSize bytes, and returns
// the root hash of the tree (lowercase hex), computed from its top level
// block: dm-verity stores the levels from the root down.
func checkTree(treePath string, deviceSize int64) (string, error) {
	f, err := os.Open(treePath) //nolint:gosec // G304: a file of the work directory
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sb, err := verity.ReadSuperblock(f, 0)
	if err != nil {
		return "", fmt.Errorf("superblock: %w", err)
	}
	want := verityParams(deviceSize)
	alg := strings.TrimRight(string(sb.Algorithm[:]), "\x00")
	switch {
	case sb.Version != 1 || sb.HashType != 1:
		return "", fmt.Errorf("superblock: version %d, hash type %d; want 1 and 1", sb.Version, sb.HashType)
	case alg != hashName:
		return "", fmt.Errorf("superblock: hash %q; want %s", alg, hashName)
	case sb.DataBlockSize != blockSize || sb.HashBlockSize != blockSize:
		return "", fmt.Errorf("superblock: blocks of %d/%d bytes; want %d/%d", sb.DataBlockSize, sb.HashBlockSize, blockSize, blockSize)
	case sb.DataBlocks != want.DataBlocks:
		return "", fmt.Errorf("superblock: %d data blocks; the device has %d", sb.DataBlocks, want.DataBlocks)
	case int(sb.SaltSize) != saltSize || !bytes.Equal(sb.Salt[:saltSize], want.Salt):
		return "", fmt.Errorf("superblock: salt is not %d zero bytes", saltSize)
	}
	top := make([]byte, blockSize)
	if _, err := f.ReadAt(top, blockSize); err != nil {
		return "", fmt.Errorf("tree: %w", err)
	}
	h := sha256.New()
	h.Write(want.Salt)
	h.Write(top)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyDevice checks every block of the device against the tree and the
// tree's root against rootHex.
func verifyDevice(devicePath string, deviceSize int64, treePath, rootHex string) error {
	root, err := hex.DecodeString(rootHex)
	if err != nil {
		return err
	}
	p := verityParams(deviceSize)
	if err := verity.Verify(&p, devicePath, treePath, root); err != nil {
		return errors.New("the device (published index ‖ layer tar) does not verify against the published tree: " + err.Error())
	}
	return nil
}
