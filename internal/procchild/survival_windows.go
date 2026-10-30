package procchild

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// Windows has no cgroup to escape and the supervisor's job object is its own, so a
// daemon stop does not touch it. An UPDATE does: the updater proves the install is
// free by stopping every process running from its exact daemon executable
// (stopWindowsExecutableProcesses, desktop/shell/update-worker.js), and a supervisor
// is that same file. So a supervisor the daemon starts runs from its own copy, which
// neither matches that search nor holds the install's file open.

var procQueryFullProcessImageName = kernel32.NewProc("QueryFullProcessImageNameW")

const processQueryLimitedInformation = 0x1000

// SurvivalOf compares the image a pid runs from with this daemon's own.
func SurvivalOf(pid int) Survival {
	own, err := os.Executable()
	if err != nil {
		return Survival{Why: "this daemon could not read its own executable path"}
	}
	image, err := processImagePath(pid)
	if err != nil {
		return Survival{Why: "its executable could not be read, so nothing can say it is safe"}
	}
	if strings.EqualFold(filepath.Clean(image), filepath.Clean(own)) {
		return Survival{Why: "it runs from the install's own executable, and the Windows updater stops every process that does"}
	}
	return Survival{Protected: true, How: "it runs from a copy the Windows updater does not stop"}
}

func processImagePath(pid int) (string, error) {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(handle)
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if ret, _, callErr := procQueryFullProcessImageName.Call(
		uintptr(handle), 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)),
	); ret == 0 {
		return "", callErr
	}
	return syscall.UTF16ToString(buffer[:size]), nil
}

// supervisorExecutableFrom makes the copy once per build. Anything going wrong
// falls back to the install's own file, which SurvivalOf then reports truthfully.
func supervisorExecutableFrom(stateDir, executable string) string {
	if stateDir == "" {
		return executable
	}
	info, err := os.Stat(executable)
	if err != nil {
		return executable
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", executable, info.Size(), info.ModTime().UnixNano())))
	dir := filepath.Join(stateDir, "supervisor-bin")
	target := filepath.Join(dir, "workass-supervisor-"+hex.EncodeToString(sum[:8])+".exe")
	if _, err := os.Stat(target); err == nil {
		return target
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return executable
	}
	if err := copyFile(executable, target); err != nil {
		return executable
	}
	// Older copies go when nothing is running from them; Windows refuses the
	// removal of one that is still in use, which is exactly the rule wanted.
	if names, err := filepath.Glob(filepath.Join(dir, "workass-supervisor-*.exe")); err == nil {
		for _, name := range names {
			if name != target {
				_ = os.Remove(name)
			}
		}
	}
	return target
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	temp, err := os.CreateTemp(filepath.Dir(target), ".supervisor-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := io.Copy(temp, in); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), target)
}

// EscapeServiceCgroup has no meaning on Windows.
func EscapeServiceCgroup(string, string, []string, io.Writer) {}

// LeaveCallerProcessGroup has no meaning on Windows, where the daemon starts the
// supervisor with a console group of its own.
func LeaveCallerProcessGroup() {}

// SupervisorEnvironmentNames are what a Windows process cannot start without.
func SupervisorEnvironmentNames() []string {
	return []string{"SystemRoot", "TEMP", "TMP", "USERPROFILE"}
}
