//go:build !darwin

package main

import (
	"errors"
	"os"
	"runtime"
)

// sessionImageClonesPossible: no clone source is ever worth looking for here.
const sessionImageClonesPossible = false

// sessionImageRawStoreSupported is off here: the raw store has only been
// proven on macOS, and Windows (and Linux) code paths are compile-checked
// only. These platforms keep 0.1.229's store exactly: new images are written
// as base64 text, and no migration, reclaim or deleted-chat collection runs.
// Turn on only after a real runtime check on the platform passes.
const sessionImageRawStoreSupported = false

// Only APFS clones a file. Elsewhere (Windows, Linux) a raw image is always
// written from the bytes already in hand, and a candidate is never read.
func cloneRawSessionImage(dir, final, name string, raw []byte, candidate string, durability sessionImageDurability) (sessionImageCloneOutcome, error) {
	return cloneUnsupported, errors.New("file clones are not supported on this platform")
}

// syncSessionImageDataToDevice: off darwin File.Sync is already per file
// (fsync, FlushFileBuffers). FlushFileBuffers needs a handle opened for
// writing (GENERIC_WRITE) and fails with ERROR_ACCESS_DENIED on a read-only
// one, so on Windows a read-only file is flushed through a second, writable
// handle to the same file (no byte is written through it).
func syncSessionImageDataToDevice(file *os.File) error {
	err := file.Sync()
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	writable, openErr := os.OpenFile(file.Name(), os.O_RDWR, 0)
	if openErr != nil {
		return errors.Join(err, openErr)
	}
	syncErr := writable.Sync()
	return errors.Join(syncErr, writable.Close())
}

func nonLocalMountPoints() []string { return nil }

// removeSessionImageTemp removes a temporary file of the store. Only darwin
// clones carry flags that can refuse it.
func removeSessionImageTemp(path string) { _ = os.Remove(path) }
