package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEnvFallbacks(t *testing.T) {
	t.Setenv("VM_MANAGER_TEST_STR", "from-env")
	t.Setenv("VM_MANAGER_TEST_EMPTY", "")
	t.Setenv("VM_MANAGER_TEST_BOOL", "true")
	t.Setenv("VM_MANAGER_TEST_BAD_BOOL", "maybe")
	t.Setenv("VM_MANAGER_TEST_INT", "4711")
	t.Setenv("VM_MANAGER_TEST_BAD_INT", "many")
	t.Setenv("VM_MANAGER_TEST_DURATION", "90s")
	t.Setenv("VM_MANAGER_TEST_BAD_DURATION", "soon")

	assert.Equal(t, "from-env", envOr("VM_MANAGER_TEST_STR", "def"))
	assert.Equal(t, "def", envOr("VM_MANAGER_TEST_EMPTY", "def"), "empty counts as unset")
	assert.Equal(t, "def", envOr("VM_MANAGER_TEST_UNSET", "def"))
	assert.True(t, envBool("VM_MANAGER_TEST_BOOL", false))
	assert.False(t, envBool("VM_MANAGER_TEST_BAD_BOOL", false), "unparsable keeps the default")
	assert.True(t, envBool("VM_MANAGER_TEST_UNSET", true))
	assert.Equal(t, 4711, envInt("VM_MANAGER_TEST_INT", 1))
	assert.Equal(t, 1, envInt("VM_MANAGER_TEST_BAD_INT", 1), "unparsable keeps the default")
	assert.Equal(t, 1, envInt("VM_MANAGER_TEST_UNSET", 1))
	assert.Equal(t, 90*time.Second, envDuration("VM_MANAGER_TEST_DURATION", time.Minute))
	assert.Equal(t, time.Minute, envDuration("VM_MANAGER_TEST_BAD_DURATION", time.Minute), "unparsable keeps the default")
	assert.Equal(t, time.Minute, envDuration("VM_MANAGER_TEST_UNSET", time.Minute))
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
	t.Setenv("HOME", "")
	assert.Equal(t, systemStateDir, defaultStateDir())
}
