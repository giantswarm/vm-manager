// Package identity carries the authenticated caller through a request: who
// the OAuth layer validated (subject, email, groups). Work that runs without a
// caller (a VM that outlives its request, host housekeeping) sees no identity
// and is logged as the service itself.
package identity

import (
	"context"
	"log/slog"
)

// Source says how the caller was authenticated.
type Source string

const (
	// SourceSSO is an IdP id_token forwarded by a trusted aggregator (muster)
	// or sent by the portal through the gateway, validated against the IdP's
	// JWKS.
	SourceSSO Source = "sso"
	// SourceOAuth is an access token issued by this server's own OAuth 2.1
	// flow (a client that authenticated directly, e.g. mcp-debug).
	SourceOAuth Source = "oauth"
)

// Identity is the authenticated caller.
type Identity struct {
	// Subject is the IdP's stable user id (the `sub` claim).
	Subject string
	// Email is the caller's email when the IdP provided one.
	Email string
	// Name is the display name when the IdP provided one.
	Name string
	// Groups are the IdP group claims (Dex `groups`; empty for Google).
	Groups []string
	// Source is how the token was validated.
	Source Source
}

// String is the caller as logged and recorded on resources: the email, else
// the subject.
func (id *Identity) String() string {
	if id == nil {
		return ""
	}
	if id.Email != "" {
		return id.Email
	}
	return id.Subject
}

type ctxKey struct{}

// ContextWith returns ctx carrying id.
func ContextWith(ctx context.Context, id *Identity) context.Context {
	if id == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the caller, if any.
func FromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(*Identity)
	return id, ok && id != nil
}

// Caller is the caller's String(), or "" without one.
func Caller(ctx context.Context) string {
	id, _ := FromContext(ctx)
	return id.String()
}

// LogAttr is the structured-log attribute every mutation carries: the caller,
// or the service marker when the operation runs without one.
func LogAttr(ctx context.Context) slog.Attr {
	if c := Caller(ctx); c != "" {
		return slog.String("caller", c)
	}
	return slog.String("caller", "vm-manager")
}
