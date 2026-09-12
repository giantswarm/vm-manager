package identity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, ok := FromContext(ctx)
	assert.False(t, ok)
	assert.Equal(t, "vm-manager", LogAttr(ctx).Value.String())
	assert.Equal(t, "", Caller(ctx))

	id := &Identity{Subject: "sub-1", Email: "admin@lab.local", Groups: []string{"platform-admins"}, Source: SourceSSO}
	ctx = ContextWith(ctx, id)
	got, ok := FromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, id, got)
	assert.Equal(t, "admin@lab.local", Caller(ctx))
	assert.Equal(t, "admin@lab.local", LogAttr(ctx).Value.String())

	assert.Equal(t, "sub-1", (&Identity{Subject: "sub-1"}).String(), "email wins, subject is the fallback")
	assert.Equal(t, "", (*Identity)(nil).String())
	assert.Same(t, ctx, ContextWith(ctx, nil), "a nil identity adds nothing")
}
