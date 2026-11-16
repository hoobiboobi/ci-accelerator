//go:build unix

package store

import "syscall"

// volumeSpace returns the bytes free (for an unprivileged writer) and in
// all on the file system that holds dir.
func volumeSpace(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil
}
