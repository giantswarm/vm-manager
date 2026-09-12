package imds

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

func TestUserDataCredentialsWrapper(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		out, err := UserDataCredentialsWrapper("sysinstall.conf", []byte(ignition))
		require.NoError(t, err)
		assert.JSONEq(t, `{"systemd.credentials":[{"name":"sysinstall.conf","text":`+quoteJSONString(ignition)+`}]}`, string(out))
	})

	t.Run("binary becomes base64 data", func(t *testing.T) {
		out, err := UserDataCredentialsWrapper("blob", []byte{0xff, 0x00, 0x01})
		require.NoError(t, err)
		var env map[string][]Credential
		require.NoError(t, json.Unmarshal(out, &env))
		require.Len(t, env[CredentialsKey], 1)
		assert.Equal(t, []byte{0xff, 0x00, 0x01}, env[CredentialsKey][0].Data)
		assert.Empty(t, env[CredentialsKey][0].Text)
		assert.Contains(t, string(out), `"data":"/wAB"`)
	})

	for _, name := range []string{"", ".", "..", "a/b", "a:b", "a b", "ä"} {
		t.Run("rejects name "+name, func(t *testing.T) {
			_, err := UserDataCredentialsWrapper(name, []byte("x"))
			assert.True(t, errors.Is(err, apierr.ErrInvalid), err)
		})
	}

	t.Run("rejects empty payload", func(t *testing.T) {
		_, err := UserDataCredentialsWrapper("x", nil)
		assert.True(t, errors.Is(err, apierr.ErrInvalid), err)
	})
}

func quoteJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
