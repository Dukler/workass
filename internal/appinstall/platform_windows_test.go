package appinstall

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReplacementProbeAllowsSharedReaderButRejectsBlockingHandle(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "library.dll")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	pointer, _ := syscall.UTF16PtrFromString(file)
	for _, tc := range []struct {
		name    string
		share   uint32
		blocked bool
	}{
		{"shared reader", syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE, false},
		{"delete blocker", syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := syscall.CreateFile(pointer, syscall.GENERIC_READ, tc.share, nil, syscall.OPEN_EXISTING, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.CloseHandle(h)
			err = probeReplacementFile(root, "library.dll")
			if (err != nil) != tc.blocked {
				t.Fatalf("blocked=%v, error=%v", tc.blocked, err)
			}
		})
	}
}

func TestReplacementProbeRejectsRunningExecutable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = probeReplacementFile(filepath.Dir(executable), filepath.Base(executable)); err == nil {
		t.Fatal("running executable passed replacement preflight")
	}
}
