//go:build darwin

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// sessionImageClonesPossible: APFS can clone; a volume that cannot reports
// ENOTSUP per attempt.
const sessionImageClonesPossible = true

// cloneRawSessionImage makes final an APFS clone of candidate when candidate
// is, byte for byte, raw (the verified content of name). It reads nothing the
// guard refuses, never crosses a volume, verifies the source before cloning
// and the clone after, and only then renames the clone into place.
func cloneRawSessionImage(dir, final, name string, raw []byte, candidate string, durability sessionImageDurability) (sessionImageCloneOutcome, error) {
	guard := sessionImageSources()
	resolved, err := guard.resolve(candidate, os.Lstat, os.Readlink)
	if err != nil {
		if errors.Is(err, errSessionImageSourceProtected) {
			return cloneProtected, err
		}
		return cloneUnavailable, err
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return cloneUnavailable, errors.New("clone source is not a regular file")
	}
	if info.Size() != int64(len(raw)) {
		return cloneDifferent, errors.New("clone source has a different size")
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return cloneUnavailable, err
	}
	sourceStat, ok := info.Sys().(*syscall.Stat_t)
	dirStat, dirOK := dirInfo.Sys().(*syscall.Stat_t)
	if !ok || !dirOK || sourceStat.Dev != dirStat.Dev {
		return cloneOtherVolume, errors.New("clone source is on another volume")
	}
	// Before: the source must be exactly these bytes.
	source, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return cloneUnavailable, err
	}
	same, err := sessionImageReaderEquals(io.LimitReader(source, int64(len(raw))+1), raw)
	_ = source.Close()
	if err != nil {
		return cloneUnavailable, err
	}
	if !same {
		return cloneDifferent, errors.New("clone source content differs")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return cloneFailed, err
	}
	tmpName := filepath.Join(dir, ".image-clone-"+hex.EncodeToString(suffix[:])+".tmp")
	if err := unix.Clonefile(resolved, tmpName, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return cloneOtherVolume, err
		}
		if errors.Is(err, unix.ENOTSUP) {
			// The store's volume cannot clone (not APFS).
			return cloneUnsupported, err
		}
		return cloneFailed, err
	}
	defer os.Remove(tmpName)
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return cloneFailed, err
	}
	if err := syncSessionImageFile(tmpName, durability); err != nil {
		return cloneFailed, err
	}
	// After: the clone is what is published, so the clone is what is checked.
	if err := verifyRawSessionImageFile(tmpName, name, int64(len(raw))); err != nil {
		return cloneFailed, err
	}
	if err := publishRawSessionImage(tmpName, final, dir, durability); err != nil {
		return cloneFailed, err
	}
	return cloneOK, nil
}

func syncSessionImageFile(path string, durability sessionImageDurability) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	syncErr := syncSessionImageData(file, durability)
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// syncSessionImageDataToDevice is a plain fsync(2): the data reaches the
// device, whose cache a later F_FULLFSYNC barrier flushes. Go's File.Sync is
// F_FULLFSYNC on darwin, a whole-device cache flush that every other writer on
// the machine waits behind; one per image made the pass felt.
func syncSessionImageDataToDevice(file *os.File) error {
	return unix.Fsync(int(file.Fd()))
}

// nonLocalMountPoints lists where network and other non-local file systems
// are mounted, from the kernel's cached table (MNT_NOWAIT never waits on a
// stalled server).
func nonLocalMountPoints() []string {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || count <= 0 {
		return nil
	}
	buffer := make([]unix.Statfs_t, count+8)
	count, err = unix.Getfsstat(buffer, unix.MNT_NOWAIT)
	if err != nil {
		return nil
	}
	var mounts []string
	for _, fs := range buffer[:count] {
		if fs.Flags&unix.MNT_LOCAL != 0 {
			continue
		}
		mount := unix.ByteSliceToString(fs.Mntonname[:])
		if mount != "" && mount != "/" {
			mounts = append(mounts, mount)
		}
	}
	return mounts
}
