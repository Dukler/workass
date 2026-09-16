package appinstall

import (
	"fmt"
	"time"
)

// Retry only the blocked file. Re-scanning thousands of already checked files
// on every retry made the apparent 30-second wait last minutes on Windows.
func waitForUnlockedFiles(names []string, probe func(string) error, retryable func(error) bool, now func() time.Time, sleep func(time.Duration)) error {
	var deadline time.Time
	for _, name := range names {
		for {
			err := probe(name)
			if err == nil {
				break
			}
			if deadline.IsZero() {
				deadline = now().Add(30 * time.Second)
			}
			if !retryable(err) || !now().Before(deadline) {
				return fmt.Errorf("cannot replace application file %q: %w; no files replaced", name, err)
			}
			sleep(250 * time.Millisecond)
		}
	}
	return nil
}
