//go:build !darwin && !linux

package acp

// Claude's structured background_tasks_changed level signal remains the
// liveness authority on platforms without an available open-file probe. The
// reconciler deliberately does not guess an exit from an unchanged file.
func spawnedWorkPIDsForOutputs([]string) (map[string][]int, bool) { return nil, false }

// Without an available socket probe there is no second signal, so nothing is
// ever classified as a service by inference here. Declaring the role at
// registration remains the whole story on these platforms.
func spawnedWorkListeningPIDs([]int) (map[int]bool, bool) { return nil, false }

func externalPIDAlive(int) bool { return true }

// Without a signal primitive there is nothing to stop, so a stop here settles
// the record and says so rather than claiming a kill it never performed.
func spawnedWorkSignalPID(int, bool) bool { return false }
