package attest

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/giantswarm/vm-manager/internal/imds"
)

// maxNoncesPerVM bounds outstanding nonces for one VM, as in
// imds.NoopAttestor: a guest that keeps asking without quoting cannot grow
// the map; the oldest nonce is dropped when the cap is reached.
const maxNoncesPerVM = 8

// nonces issues single-use nonces bound to a VM with imds.NonceTTL; the
// semantics mirror imds.NoopAttestor so the guest sees one contract.
type nonces struct {
	now func() time.Time

	mu   sync.Mutex
	byVM map[string]map[string]time.Time // vmID -> nonce -> issued
}

// issue returns 32 random bytes, hex-encoded, valid for vmID.
func (n *nonces) issue(vmID string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(raw[:])

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.byVM == nil {
		n.byVM = make(map[string]map[string]time.Time)
	}
	now := n.now()
	n.sweepLocked(now)
	if n.byVM[vmID] == nil {
		n.byVM[vmID] = make(map[string]time.Time)
	}
	for len(n.byVM[vmID]) >= maxNoncesPerVM {
		delete(n.byVM[vmID], oldest(n.byVM[vmID]))
	}
	n.byVM[vmID][nonce] = now
	return nonce, nil
}

// consume reports whether nonce was issued to vmID within the TTL and
// forgets it either way.
func (n *nonces) consume(vmID, nonce string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	issued, ok := n.byVM[vmID][nonce]
	if ok {
		delete(n.byVM[vmID], nonce)
	}
	n.sweepLocked(now)
	return ok && now.Sub(issued) <= imds.NonceTTL
}

// forget drops every nonce of a VM.
func (n *nonces) forget(vmID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.byVM, vmID)
}

func (n *nonces) sweepLocked(now time.Time) {
	for vmID, issued := range n.byVM {
		for nonce, t := range issued {
			if now.Sub(t) > imds.NonceTTL {
				delete(issued, nonce)
			}
		}
		if len(issued) == 0 {
			delete(n.byVM, vmID)
		}
	}
}

func oldest(issued map[string]time.Time) string {
	var (
		key string
		at  time.Time
	)
	for nonce, t := range issued {
		if key == "" || t.Before(at) {
			key, at = nonce, t
		}
	}
	return key
}
