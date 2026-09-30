//go:build unix

package daemon

import (
	"os/exec"
	"syscall"
)

// terminalSupported reports whether this platform can host a PTY-backed
// shell. Unix always can.
func terminalSupported() bool { return true }

// terminateTerminal asks the shell's process group to hang up. pty.Start
// sets Setsid, so the shell leads its own group and a signal to -pid reaches
// every job it started.
func terminateTerminal(cmd *exec.Cmd) { signalTerminal(cmd, syscall.SIGHUP) }

// killTerminal force-kills the shell's process group after the grace period.
func killTerminal(cmd *exec.Cmd) { signalTerminal(cmd, syscall.SIGKILL) }

func signalTerminal(cmd *exec.Cmd, signal syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, signal); err != nil {
		// Nothing left to signal means the shell already exited; signalling
		// the process itself is the fallback for a group-less command.
		_ = cmd.Process.Signal(signal)
	}
}
