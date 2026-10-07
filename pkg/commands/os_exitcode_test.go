package commands

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRunCommandWithExitCode verifies the subprocess execution path can
// recover the process exit code (the ledger records it per process entry).
func TestRunCommandWithExitCode(t *testing.T) {
	osCommand := NewDummyOSCommand()

	success := "sh -c 'exit 0'"
	failure := "sh -c 'exit 3'"
	if runtime.GOOS == "windows" {
		success = "cmd /c exit 0"
		failure = "cmd /c exit 3"
	}

	code, err := osCommand.RunCommandWithExitCode(success)
	assert.NoError(t, err)
	assert.Equal(t, 0, code)

	code, err = osCommand.RunCommandWithExitCode(failure)
	assert.Error(t, err)
	assert.Equal(t, 3, code)

	// a process that cannot be started reports -1 rather than a real code
	code, err = osCommand.RunCommandWithExitCode(filepath.Join(t.TempDir(), "no-such-binary"))
	assert.Error(t, err)
	assert.Equal(t, -1, code)

	// ExitCode helper
	assert.Equal(t, 0, ExitCode(nil))
	assert.Equal(t, -1, ExitCode(fmt.Errorf("not an exit error")))
}
