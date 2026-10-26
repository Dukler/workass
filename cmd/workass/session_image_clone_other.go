//go:build !darwin

package main

import (
	"errors"
	"os"
)

// sessionImageClonesPossible: no clone source is ever worth looking for here.
const sessionImageClonesPossible = false

// Only APFS clones a file. Elsewhere (Windows, Linux) a raw image is always
// written from the bytes already in hand, and a candidate is never read.
func cloneRawSessionImage(dir, final, name string, raw []byte, candidate string, durability sessionImageDurability) (sessionImageCloneOutcome, error) {
	return cloneUnsupported, errors.New("file clones are not supported on this platform")
}

// syncSessionImageDataToDevice: off darwin File.Sync is already per file
// (fsync, FlushFileBuffers).
func syncSessionImageDataToDevice(file *os.File) error { return file.Sync() }

func nonLocalMountPoints() []string { return nil }
