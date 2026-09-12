// Package nonce is the freshness half of the attestation handshake: the
// IMDS issues a nonce per GET /attest/nonce and a quote must carry it back
// as its qualifying data. A Store keeps the outstanding nonces per VM so one
// nonce proves three things at once: it was issued to this VM, it is younger
// than the TTL and it has not been used before. imds.NoopAttestor and
// attest.Verifier are both built on it, so the guest sees one contract
// whichever Attestor answers.
package nonce

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Defaults of a Store built without the matching Option.
const (
	// DefaultTTL is how long an issued nonce stays valid.
	DefaultTTL = 5 * time.Minute
	// DefaultMaxPerVM bounds the outstanding nonces of one VM so a guest that
	// keeps asking without ever quoting cannot grow the store without limit;
	// the oldest nonce is dropped when the cap is reached.
	DefaultMaxPerVM = 8
)

// Option configures a Store; see New.
type Option func(*Store)

// WithTTL sets how long an issued nonce stays valid; d <= 0 keeps DefaultTTL.
func WithTTL(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.ttl = d
		}
	}
}

// WithMaxPerVM caps the outstanding nonces per VM; n <= 0 keeps
// DefaultMaxPerVM.
func WithMaxPerVM(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.maxPerVM = n
		}
	}
}

// WithClock sets the clock issue and expiry are measured with, for tests;
// nil keeps time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// Store issues single-use nonces bound to a VM. Build it with New. It is
// safe for concurrent use; expired nonces are swept on every call, so a VM
// that stopped quoting does not keep memory.
type Store struct {
	ttl      time.Duration
	maxPerVM int
	now      func() time.Time

	mu   sync.Mutex
	byVM map[string]map[string]time.Time // vmID -> nonce -> issued
}

// New returns an empty Store with the defaults, adjusted by opts.
func New(opts ...Option) *Store {
	s := &Store{ttl: DefaultTTL, maxPerVM: DefaultMaxPerVM, now: time.Now, byVM: make(map[string]map[string]time.Time)}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Issue returns a fresh nonce for vmID: 32 random bytes, hex-encoded. When
// the VM already has the maximum outstanding, the oldest is dropped first.
func (s *Store) Issue(vmID string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(raw[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	issued := s.byVM[vmID]
	if issued == nil {
		issued = make(map[string]time.Time)
		s.byVM[vmID] = issued
	}
	for len(issued) >= s.maxPerVM {
		delete(issued, oldest(issued))
	}
	issued[nonce] = now
	return nonce, nil
}

// Consume reports whether nonce was issued to vmID within the TTL and
// forgets it either way: a nonce is accepted at most once.
func (s *Store) Consume(vmID, nonce string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	issued, ok := s.byVM[vmID][nonce]
	if ok {
		delete(s.byVM[vmID], nonce)
	}
	s.sweepLocked(now)
	return ok && now.Sub(issued) <= s.ttl
}

// Forget drops every nonce of a VM.
func (s *Store) Forget(vmID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byVM, vmID)
}

// sweepLocked drops expired nonces and empty per-VM maps; callers hold s.mu.
func (s *Store) sweepLocked(now time.Time) {
	for vmID, issued := range s.byVM {
		for nonce, t := range issued {
			if now.Sub(t) > s.ttl {
				delete(issued, nonce)
			}
		}
		if len(issued) == 0 {
			delete(s.byVM, vmID)
		}
	}
}

// oldest returns the nonce with the earliest issue time; issued is not empty.
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
