//go:build !windows

package procchild

import "os"

// A process whose parent exits is re-parented (to init, launchd or a
// subreaper), so a parent pid that is still the one captured at start is the
// same live process: it cannot have been reused while this one is its child.
func watchCaller() func() bool {
	parent := os.Getppid()
	discarded := stdoutDiscarded()
	return func() bool {
		return parent > 1 && !discarded && os.Getppid() == parent
	}
}

// stdoutDiscarded reports a receipt written to the null device: the caller
// asked not to read it, so the outcome reaches it only through the chat.
func stdoutDiscarded() bool {
	out, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	null, err := os.Stat(os.DevNull)
	if err != nil {
		return false
	}
	return os.SameFile(out, null)
}
