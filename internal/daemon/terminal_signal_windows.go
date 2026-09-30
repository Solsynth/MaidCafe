//go:build windows

package daemon

import "os/exec"

// terminalSupported reports whether this platform can host a PTY-backed
// shell. Windows has no PTY, so the endpoint answers 501 and pty.StartWithSize
// would return ErrUnsupported anyway.
func terminalSupported() bool { return false }

// terminateTerminal has no process group to hang up on Windows; killing the
// shell is the only available release.
func terminateTerminal(cmd *exec.Cmd) { killTerminal(cmd) }

// killTerminal kills the shell process.
func killTerminal(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
