//go:build unix

package agent

import "syscall"

// diskSpace returns free and total bytes on the filesystem holding path.
// Free is what an unprivileged writer can use (Bavail), like df shows.
func diskSpace(path string) (free, total int64) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0
	}
	bs := int64(s.Bsize) //nolint:unconvert // Bsize's type differs between platforms
	return int64(s.Bavail) * bs, int64(s.Blocks) * bs
}
