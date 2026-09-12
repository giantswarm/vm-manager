package vm

// Internals the external tests reach into.
const (
	VMsDir           = vmsDir
	RecordFile       = recordFile
	NetworksFile     = networksFile
	ConsoleTailLines = consoleTailLines
	HostKeyFile      = hostKeyFile
	LockFile         = lockFile
)

var WriteJSON = writeJSON

var GenerateSSHKey = generateSSHKey

var LoadSigner = loadSigner

// LockOp takes the VM's operation lock, as an in-flight Create, Start or
// Delete would; the returned func releases it.
func (s *Service) LockOp(id string) (func(), error) {
	e, err := s.entry(id)
	if err != nil {
		return nil, err
	}
	e.opMu.Lock()
	return e.opMu.Unlock, nil
}

// Closing reports whether Close has begun.
func (s *Service) Closing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}
