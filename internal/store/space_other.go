//go:build !unix

package store

import "errors"

// volumeSpace is not known on this system.
func volumeSpace(string) (free, total uint64, err error) {
	return 0, 0, errors.ErrUnsupported
}
