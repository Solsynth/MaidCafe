//go:build windows

package privfs

import "io/fs"

// fileOwner has no meaning on Windows, where the helper is unusable anyway:
// there is no sudo, no root account and no Unix permission bits. Reporting
// false makes the ownership requirement unsatisfiable, so the production
// loader refuses to run rather than running without the check.
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
