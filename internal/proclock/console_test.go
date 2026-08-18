package proclock

import (
	"os/exec"
	"runtime"
	"testing"
)

// Everywhere else this must cost nothing and change nothing: a Unix process has no console,
// and a SysProcAttr set here would silently override whatever the caller meant to set.
func TestNoConsoleDoesNothingOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this is the platform where NoConsole does something")
	}
	cmd := &exec.Cmd{}
	NoConsole(cmd)
	if cmd.SysProcAttr != nil {
		t.Errorf("NoConsole set SysProcAttr to %+v on %s", cmd.SysProcAttr, runtime.GOOS)
	}
}
