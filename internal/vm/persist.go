package vm

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/giantswarm/vm-manager/internal/network"
)

// tailReadBytes bounds how much of the console is read for a tail.
const tailReadBytes = 64 * 1024

// persist writes the record; the caller holds s.mu.
func (s *Service) persist(e *entry) error {
	if err := writeJSON(filepath.Join(e.rec.Paths.Dir, recordFile), &e.rec); err != nil {
		return fmt.Errorf("persist vm %s: %w", e.rec.ID, err)
	}
	return nil
}

// save is persist for paths that cannot return the error.
func (s *Service) save(e *entry) {
	if err := s.persist(e); err != nil {
		s.log.Error("persist vm record", "id", e.rec.ID, "err", err)
	}
}

// persistNetworks writes the network specs and leases.
func (s *Service) persistNetworks() error {
	states := s.opts.Networks.States()
	if states == nil {
		states = []network.State{}
	}
	if err := writeJSON(filepath.Join(s.opts.StateDir, networksFile), states); err != nil {
		return fmt.Errorf("persist networks: %w", err)
	}
	return nil
}

func (s *Service) loadNetworks() ([]network.State, error) {
	var states []network.State
	err := readJSON(filepath.Join(s.opts.StateDir, networksFile), &states)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load networks: %w", err)
	}
	return states, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path) // #nosec G304 -- state dir file.
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// writeAtomic writes data to a temporary file next to path and renames it
// into place, so readers never see a partial file.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	err = func() error {
		defer func() { _ = tmp.Close() }()
		if _, err := tmp.Write(data); err != nil {
			return err
		}
		if err := tmp.Chmod(perm); err != nil {
			return err
		}
		return tmp.Sync()
	}()
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// copyFile copies a regular file (the OVMF variable store template).
func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- configured firmware path.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	return writeAtomic(dst, data, 0o600)
}

// tailLines returns the last n lines of the file, reading at most the final
// tailReadBytes.
func tailLines(path string, n int) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- state dir file.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	offset := st.Size() - tailReadBytes
	if offset < 0 {
		offset = 0
	}
	data, err := io.ReadAll(io.NewSectionReader(f, offset, st.Size()-offset))
	if err != nil {
		return "", err
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return "", nil
	}
	lines := strings.Split(text, "\n")
	if offset > 0 && len(lines) > 1 {
		lines = lines[1:] // the first line is cut mid-way
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
