package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServerConfigEnablesClientIDMetadataDocuments(t *testing.T) {
	require.True(t, oauthServerConfig(OAuthConfig{}, nil).EnableClientIDMetadataDocuments)
}
