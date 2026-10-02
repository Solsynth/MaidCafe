//go:build !windows

// Command maidkit-priv is the privileged file helper: the only component in
// the MaidCafe stack that runs as root.
//
// The daemon invokes it through a passwordless sudoers rule for each mutating
// verb, and the helper resolves the directory it may touch from a root-owned
// profile file rather than from its arguments. See the package documentation
// in internal/privfs for why the boundary lives here instead of in the
// sudoers pattern.
//
// It is excluded from Windows builds: there is no sudo, no root account and no
// Unix permission bits to enforce there.
package main

import (
	"os"

	"src.solsynth.dev/solsynth/maidcafe/internal/privfs"
)

func main() {
	os.Exit(privfs.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, privfs.Env{
		EUID: os.Geteuid(),
		// sudo exports the invoking account, so the audit line names the real
		// caller instead of one the command line could claim.
		Invoker:    os.Getenv("SUDO_USER"),
		InvokerUID: os.Getenv("SUDO_UID"),
	}))
}
