package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEnvFallbacks(t *testing.T) {
	t.Setenv("VM_MANAGER_TEST_STR", "from-env")
	t.Setenv("VM_MANAGER_TEST_EMPTY", "")
	t.Setenv("VM_MANAGER_TEST_BOOL", "true")
	t.Setenv("VM_MANAGER_TEST_BAD_BOOL", "maybe")

	assert.Equal(t, "from-env", envOr("VM_MANAGER_TEST_STR", "def"))
	assert.Equal(t, "def", envOr("VM_MANAGER_TEST_EMPTY", "def"), "empty counts as unset")
	assert.Equal(t, "def", envOr("VM_MANAGER_TEST_UNSET", "def"))
	assert.True(t, envBool("VM_MANAGER_TEST_BOOL", false))
	assert.False(t, envBool("VM_MANAGER_TEST_BAD_BOOL", false), "unparsable keeps the default")
	assert.True(t, envBool("VM_MANAGER_TEST_UNSET", true))
}

func TestSplitList(t *testing.T) {
	assert.Nil(t, splitList(""))
	assert.Equal(t, []string{"a", "b"}, splitList(" a, ,b,"))
}

func TestDefaultStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdg")
	assert.Equal(t, "/xdg/vm-manager", defaultStateDir())
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	assert.Equal(t, "/home/u/.local/state/vm-manager", defaultStateDir())
}
