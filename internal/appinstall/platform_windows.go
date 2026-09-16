package appinstall

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func supportedPlatform() error { return nil }

func retryableRename(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, syscall.Errno(32))
}

func platformOperations(p Plan) (operations, func(), error) {
	// Hold the process handle before arming; PID reuse cannot redirect the wait.
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(p.ShellPID))
	if err != nil {
		return operations{}, nil, err
	}
	return operations{
		waitShell: func() error {
			status, err := syscall.WaitForSingleObject(handle, 30000)
			if err != nil {
				return err
			}
			if status != syscall.WAIT_OBJECT_0 {
				return errors.New("Workass did not close; application files unchanged")
			}
			return nil
		},
		waitFiles: waitForFiles,
		launch: func(executable string) error {
			cmd := exec.Command(executable)
			cmd.Dir = filepath.Dir(executable)
			// Resolve the installed profile normally, not the incoming helper's
			// inherited Electron/Node or old update-recovery environment.
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				upper := strings.ToUpper(key)
				if strings.HasPrefix(upper, "WORKASS_") || upper == "ELECTRON_RUN_AS_NODE" || upper == "NODE_OPTIONS" {
					continue
				}
				cmd.Env = append(cmd.Env, entry)
			}
			cmd.Env = append(cmd.Env, "WORKASS_UPDATE_RELAUNCH=1", "WORKASS_DATA_ROOT="+p.DataRoot)
			cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | syscall.CREATE_NEW_PROCESS_GROUP}
			if err := cmd.Start(); err != nil {
				return err
			}
			return cmd.Process.Release()
		},
	}, func() { syscall.CloseHandle(handle) }, nil
}

func waitForFiles(root string, names []string) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		var blocked error
		for _, name := range names {
			file := filepath.Join(root, filepath.FromSlash(name))
			pointer, err := syscall.UTF16PtrFromString(file)
			if err != nil {
				return err
			}
			h, err := syscall.CreateFile(pointer, syscall.GENERIC_READ|syscall.GENERIC_WRITE|0x10000, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
			if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
				continue
			}
			if err != nil {
				blocked = err
				break
			}
			syscall.CloseHandle(h)
		}
		if blocked == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("Workass application files remain locked or access was denied; no files replaced")
		}
		time.Sleep(250 * time.Millisecond)
	}
}
