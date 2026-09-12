// Package apierr holds the sentinel errors every domain package wraps so the
// API layer can map them to one HTTP status and one stable error code for
// REST and MCP alike (internal/api statusFor). Wrap with fmt.Errorf("%w: ...").
package apierr

import "errors"

var (
	// ErrNotFound is a VM, network, image or host resource that does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalid is a request the caller can fix: missing or malformed fields.
	ErrInvalid = errors.New("invalid request")
	// ErrConflict is an operation the resource's current state does not allow.
	ErrConflict = errors.New("conflict")
	// ErrUnsupported is a capability this host or configuration does not offer.
	ErrUnsupported = errors.New("unsupported")
)
