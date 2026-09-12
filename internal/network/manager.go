package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

// networksDir is the subdirectory of the state dir holding one directory per
// network.
const networksDir = "networks"

// Manager owns the networks of one vm-manager process.
type Manager struct {
	log      *slog.Logger
	stateDir string

	mu   sync.Mutex
	nets map[string]*Network
}

// NewManager returns a manager keeping network sockets under
// stateDir/networks/<name>/. A nil logger means slog.Default().
func NewManager(stateDir string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, stateDir: stateDir, nets: make(map[string]*Network)}
}

// Dir is the directory of the named network.
func (m *Manager) Dir(name string) string {
	return filepath.Join(m.stateDir, networksDir, name)
}

// Create brings up a new network. It fails with apierr.ErrInvalid for a bad
// spec and apierr.ErrConflict when the name is taken.
func (m *Manager) Create(ctx context.Context, spec Spec) (*Network, error) {
	return m.add(ctx, spec, nil)
}

// Restore rebuilds networks from persisted state, keeping their leases. It
// stops at the first failure and reports which network it could not restore;
// networks restored before it stay up.
func (m *Manager) Restore(ctx context.Context, states []State) error {
	for _, s := range states {
		if _, err := m.add(ctx, s.Spec, s.Leases); err != nil {
			return fmt.Errorf("restore network %q: %w", s.Spec.Name, err)
		}
	}
	return nil
}

func (m *Manager) add(ctx context.Context, spec Spec, leases map[string]string) (*Network, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.nets[spec.Name]; exists {
		return nil, fmt.Errorf("%w: network %q already exists", apierr.ErrConflict, spec.Name)
	}
	n, err := newNetwork(ctx, m.Dir(spec.Name), spec, leases, m.log)
	if err != nil {
		return nil, err
	}
	m.nets[spec.Name] = n
	return n, nil
}

// Get returns the named network or apierr.ErrNotFound.
func (m *Manager) Get(name string) (*Network, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nets[name]
	if !ok {
		return nil, fmt.Errorf("%w: network %q", apierr.ErrNotFound, name)
	}
	return n, nil
}

// List returns all networks sorted by name.
func (m *Manager) List() []*Network {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Network, 0, len(m.nets))
	for _, n := range m.nets {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// States snapshots every network for persistence, sorted by name.
func (m *Manager) States() []State {
	nets := m.List()
	out := make([]State, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.State())
	}
	return out
}

// Delete tears a network down and removes its directory. A network with
// attached VMs is not deleted (apierr.ErrConflict); detach them first.
func (m *Manager) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nets[name]
	if !ok {
		return fmt.Errorf("%w: network %q", apierr.ErrNotFound, name)
	}
	if attached := len(n.Leases()); attached > 0 {
		return fmt.Errorf("%w: network %q has %d attached VMs", apierr.ErrConflict, name, attached)
	}
	err := n.Close()
	if rmErr := os.RemoveAll(n.dir); rmErr != nil {
		err = errors.Join(err, rmErr)
	}
	delete(m.nets, name)
	return err
}

// Close shuts every network down without removing state on disk.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var err error
	for name, n := range m.nets {
		err = errors.Join(err, n.Close())
		delete(m.nets, name)
	}
	return err
}
