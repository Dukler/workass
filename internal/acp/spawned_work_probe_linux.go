package acp

import "os/exec"

func spawnedWorkProbeAvailable() bool {
	_, err := exec.LookPath("lsof")
	return err == nil
}

func spawnedWorkPIDsForOutputs(paths []string) (map[string][]int, bool) {
	return spawnedWorkPIDsForOutputsLsof(paths)
}
