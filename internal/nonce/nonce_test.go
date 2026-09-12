package nonce

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	vm1 = "vm-1"
	vm2 = "vm-2"
)

// clock is a settable clock for a Store under test.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newStore(opts ...Option) (*Store, *clock) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	return New(append([]Option{WithClock(c.Now)}, opts...)...), c
}

func issue(t *testing.T, s *Store, vmID string) string {
	t.Helper()
	n, err := s.Issue(vmID)
	require.NoError(t, err)
	return n
}

func TestIssueAndConsume(t *testing.T) {
	s, _ := newStore()

	n := issue(t, s, vm1)
	assert.Len(t, n, 64, "32 random bytes, hex-encoded")
	_, err := hex.DecodeString(n)
	require.NoError(t, err)
	assert.NotEqual(t, n, issue(t, s, vm1), "every nonce is distinct")

	assert.True(t, s.Consume(vm1, n))
	assert.False(t, s.Consume(vm1, n), "nonces are single-use")
	assert.False(t, s.Consume(vm1, "never-issued"))
}

func TestBoundToVM(t *testing.T) {
	s, _ := newStore()

	n := issue(t, s, vm1)
	assert.False(t, s.Consume(vm2, n), "a nonce only counts for the VM it was issued to")
	assert.True(t, s.Consume(vm1, n), "a failed attempt from another VM does not burn it")
}

func TestExpiry(t *testing.T) {
	s, c := newStore()

	n := issue(t, s, vm1)
	c.now = c.now.Add(DefaultTTL)
	assert.True(t, s.Consume(vm1, n), "valid up to and including the TTL")

	n = issue(t, s, vm1)
	c.now = c.now.Add(DefaultTTL + time.Second)
	assert.False(t, s.Consume(vm1, n), "expired after the TTL")

	s, c = newStore(WithTTL(time.Second))
	n = issue(t, s, vm1)
	c.now = c.now.Add(2 * time.Second)
	assert.False(t, s.Consume(vm1, n), "WithTTL shortens the window")
}

func TestSweep(t *testing.T) {
	s, c := newStore()

	issue(t, s, vm1)
	issue(t, s, vm2)
	c.now = c.now.Add(DefaultTTL + time.Second)

	assert.False(t, s.Consume(vm1, "unknown"))
	assert.Empty(t, s.byVM, "consume sweeps expired nonces and empty VMs")

	issue(t, s, vm1)
	c.now = c.now.Add(DefaultTTL + time.Second)
	issue(t, s, vm2)
	assert.Len(t, s.byVM, 1, "issue sweeps the other VM's expired nonce")
	assert.Contains(t, s.byVM, vm2)
}

func TestCapPerVM(t *testing.T) {
	s, c := newStore()
	first := issue(t, s, vm1)
	for i := 0; i < DefaultMaxPerVM+2; i++ {
		c.now = c.now.Add(time.Second)
		issue(t, s, vm1)
	}
	assert.Len(t, s.byVM[vm1], DefaultMaxPerVM, "outstanding nonces per VM are capped")
	assert.False(t, s.Consume(vm1, first), "the oldest nonce is evicted first")

	s, c = newStore(WithMaxPerVM(2))
	a := issue(t, s, vm1)
	other := issue(t, s, vm2)
	c.now = c.now.Add(time.Second)
	b := issue(t, s, vm1)
	c.now = c.now.Add(time.Second)
	d := issue(t, s, vm1)
	assert.False(t, s.Consume(vm1, a), "evicted by the third issue")
	assert.True(t, s.Consume(vm1, b))
	assert.True(t, s.Consume(vm1, d))
	assert.True(t, s.Consume(vm2, other), "the cap applies per VM")
}

func TestForget(t *testing.T) {
	s, _ := newStore()
	n1 := issue(t, s, vm1)
	n2 := issue(t, s, vm2)

	s.Forget(vm1)
	assert.False(t, s.Consume(vm1, n1))
	assert.True(t, s.Consume(vm2, n2), "forget is per VM")
	s.Forget("never-seen")
}

func TestOptionsKeepDefaultsOnZero(t *testing.T) {
	s := New(WithTTL(0), WithMaxPerVM(0), WithClock(nil))
	assert.Equal(t, DefaultTTL, s.ttl)
	assert.Equal(t, DefaultMaxPerVM, s.maxPerVM)
	assert.NotNil(t, s.now)
}
