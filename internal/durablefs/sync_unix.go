//go:build !windows

package durablefs

import (
	"errors"
	"os"
)

// SyncDirectory commits a preceding rename into path on filesystems that
// expose directory fsync through os.File.Sync.
func SyncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
