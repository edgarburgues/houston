//go:build !windows

package provision

import "os"

// makeLink creates a directory symlink at link pointing to target (junctions
// are Windows-only; on POSIX a plain symlink does the job).
func makeLink(link, target string) error { return os.Symlink(target, link) }
