package appinstall

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFileWaitRetriesOnlyBlockedFileAndNamesFailure(t *testing.T) {
	clock := time.Unix(1, 0)
	calls := map[string]int{}
	lock := errors.New("sharing violation")
	err := waitForUnlockedFiles([]string{"first.dll", "busy.dll", "last.dll"}, func(name string) error {
		calls[name]++
		if name == "busy.dll" {
			return lock
		}
		return nil
	}, func(error) bool { return true }, func() time.Time { return clock }, func(d time.Duration) { clock = clock.Add(d) })
	if !errors.Is(err, lock) || !strings.Contains(err.Error(), `"busy.dll"`) {
		t.Fatalf("missing exact blocker: %v", err)
	}
	if calls["first.dll"] != 1 || calls["last.dll"] != 0 {
		t.Fatalf("rescanned files or continued after failure: %v", calls)
	}
	if clock.Sub(time.Unix(1, 0)) != 30*time.Second {
		t.Fatalf("wait exceeded bound: %v", clock)
	}
}

func TestFileWaitContinuesWhenLockReleased(t *testing.T) {
	clock := time.Unix(1, 0)
	calls := 0
	err := waitForUnlockedFiles([]string{"busy.dll"}, func(string) error {
		calls++
		if calls == 1 {
			return errors.New("locked")
		}
		return nil
	}, func(error) bool { return true }, func() time.Time { return clock }, func(d time.Duration) { clock = clock.Add(d) })
	if err != nil || calls != 2 {
		t.Fatalf("lock release: %d %v", calls, err)
	}
}

func TestFileWaitPermanentErrorFailsImmediately(t *testing.T) {
	denied := errors.New("invalid path")
	err := waitForUnlockedFiles([]string{"invalid.dll"}, func(string) error { return denied }, func(error) bool { return false }, time.Now, func(time.Duration) { t.Fatal("retried permanent failure") })
	if !errors.Is(err, denied) {
		t.Fatal(err)
	}
}
