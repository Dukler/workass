//go:build windows

package httpserve

// Windows has no getrusage. The production Windows host reads working-set size
// through its own tooling; the runtime heap counters above still apply.
func peakRSSBytes() (uint64, bool) {
	return 0, false
}
