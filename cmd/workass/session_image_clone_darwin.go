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

// sessionImageRawStoreSupported: the raw store, its migration, reclaim and
// deleted-chat collection were proven on macOS against a copy of a real
// store. See session_image_clone_other.go for the other platforms.
const sessionImageRawStoreSupported = true

// cloneRawSessionImage makes final an APFS clone of candidate when candidate
// is, byte for byte, raw (the verified content of name).
//
// The candidate is opened once (openSessionImageSource) and everything after
// that is about that descriptor: its kind, size and volume come from fstat,
// its bytes are compared through it, and the clone is made from it
// (fclonefileat). No second lookup by name can land on anything the walk did
// not check: an intermediate directory swapped for a symlink, or the file for
// a FIFO, after the check is never followed or opened.
func cloneRawSessionImage(dir, final, name string, raw []byte, candidate string, durability sessionImageDurability) (sessionImageCloneOutcome, error) {
	source, outcome, err := openSessionImageSource(sessionImageSources(), candidate)
	if err != nil {
		return outcome, err
	}
	defer source.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &stat); err != nil {
		return cloneUnavailable, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return cloneUnavailable, errors.New("clone source is not a regular file")
	}
	if stat.Size != int64(len(raw)) {
		return cloneDifferent, errors.New("clone source has a different size")
	}
	// A clone carries its source's BSD flags. A system immutable, append-only
	// or no-unlink flag is one this process could never clear on the clone,
	// which would then be impossible to remove: such a source is not cloned.
	if stat.Flags&(unix.SF_IMMUTABLE|unix.SF_APPEND|unix.SF_NOUNLINK) != 0 {
		return cloneUnavailable, errors.New("clone source has system file flags")
	}
	var dirStat unix.Stat_t
	if err := unix.Stat(dir, &dirStat); err != nil {
		return cloneUnavailable, err
	}
	if stat.Dev != dirStat.Dev {
		return cloneOtherVolume, errors.New("clone source is on another volume")
	}
	// Before: the source must be exactly these bytes.
	same, err := sessionImageReaderEquals(io.LimitReader(source, int64(len(raw))+1), raw)
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
	if err := unix.Fclonefileat(int(source.Fd()), unix.AT_FDCWD, tmpName, unix.CLONE_NOOWNERCOPY); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return cloneOtherVolume, err
		}
		if errors.Is(err, unix.ENOTSUP) {
			// The store's volume cannot clone (not APFS).
			return cloneUnsupported, err
		}
		return cloneFailed, err
	}
	defer removeSessionImageTemp(tmpName)
	// The clone is this process's own file: drop a locked (uchg) or
	// append-only source's user flags before anything else touches it.
	if err := clearSessionImageLockFlags(tmpName); err != nil {
		return cloneFailed, err
	}
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

// openSessionImageSource opens candidate for reading without ever looking at
// anything the guard refuses. It walks the path one component at a time from
// a descriptor of the directory already reached: each step is classified as a
// path first, then examined (fstatat without following) and opened relative
// to that directory with O_NOFOLLOW, so a component that turns into a symlink
// in between fails the open instead of redirecting it. A symlink is followed
// only after its target is classified; the walk then restarts from the root.
// The file itself is opened non-blocking (a FIFO swapped in never blocks the
// caller, who holds the store's lock) and must be a regular file.
func openSessionImageSource(guard sessionImageSourceGuard, candidate string) (*os.File, sessionImageCloneOutcome, error) {
	refuse := func(err error) (*os.File, sessionImageCloneOutcome, error) {
		if errors.Is(err, errSessionImageSourceProtected) {
			return nil, cloneProtected, err
		}
		return nil, cloneUnavailable, err
	}
	if err := guard.check(candidate); err != nil {
		return refuse(err)
	}
	remaining := splitSessionImagePath(filepath.Clean(candidate))
	resolved := "/"
	dirfd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return refuse(err)
	}
	closeDir := func() {
		if dirfd >= 0 {
			_ = unix.Close(dirfd)
			dirfd = -1
		}
	}
	defer closeDir()
	for hops := 0; len(remaining) > 0; {
		component := remaining[0]
		remaining = remaining[1:]
		if component == ".." {
			// Unreachable: the path and every symlink target are cleaned
			// first. The walk only ever descends from a checked directory.
			return refuse(errors.New("clone source path climbs"))
		}
		next := filepath.Join(resolved, component)
		if err := guard.check(next); err != nil {
			return refuse(err)
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(dirfd, component, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return refuse(err)
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			if hops++; hops > 16 {
				return refuse(errors.New("clone source has too many symlinks"))
			}
			buffer := make([]byte, 4096)
			n, err := unix.Readlinkat(dirfd, component, buffer)
			if err != nil {
				return refuse(err)
			}
			target := string(buffer[:n])
			if !filepath.IsAbs(target) {
				target = filepath.Join(resolved, target)
			}
			target = filepath.Clean(target)
			if err := guard.check(target); err != nil {
				return refuse(err)
			}
			remaining = append(splitSessionImagePath(target), remaining...)
			resolved = "/"
			closeDir()
			if dirfd, err = openSessionImageDir("/"); err != nil {
				return refuse(err)
			}
		case unix.S_IFDIR:
			if len(remaining) == 0 {
				return refuse(errors.New("clone source is a directory"))
			}
			fd, err := unix.Openat(dirfd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return refuse(err)
			}
			closeDir()
			dirfd, resolved = fd, next
		case unix.S_IFREG:
			if len(remaining) != 0 {
				return refuse(errors.New("clone source path runs through a file"))
			}
			fd, err := unix.Openat(dirfd, component, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err != nil {
				return refuse(err)
			}
			var opened unix.Stat_t
			if err := unix.Fstat(fd, &opened); err != nil || opened.Mode&unix.S_IFMT != unix.S_IFREG {
				_ = unix.Close(fd)
				return refuse(errors.New("clone source is not a regular file"))
			}
			if err := unix.SetNonblock(fd, false); err != nil {
				_ = unix.Close(fd)
				return refuse(err)
			}
			return os.NewFile(uintptr(fd), next), cloneOK, nil
		default:
			return refuse(errors.New("clone source is not a regular file"))
		}
	}
	return refuse(errors.New("clone source path is empty"))
}

// sessionImageLockFlags are the user flags that stop a file being changed
// or removed; its owner may clear them.
const sessionImageLockFlags = unix.UF_IMMUTABLE | unix.UF_APPEND

func clearSessionImageLockFlags(path string) error {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return err
	}
	if stat.Flags&sessionImageLockFlags == 0 {
		return nil
	}
	// Only ever a temporary file this process created (Lstat above saw no
	// symlink), so following is not a concern.
	return unix.Chflags(path, int(stat.Flags&^sessionImageLockFlags))
}

// removeSessionImageTemp removes a temporary file of the store, clearing
// lock flags first when a plain remove is refused.
func removeSessionImageTemp(path string) {
	if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
		return
	}
	if clearSessionImageLockFlags(path) == nil {
		_ = os.Remove(path)
	}
}

func openSessionImageDir(path string) (int, error) {
	return unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC, 0)
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
