//go:build darwin || linux

package acp

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func externalPIDAlive(pid int) bool {
	if pid <= 1 {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// spawnedWorkSignalPID asks one process to stop, and reports whether the signal
// was accepted. Never a process group: the pid set a stop works from is
// discovered by open-file probe, so it already contains every process of the
// lane, and negating it would widen the blast radius to whatever else shares
// the session's group — including, for a lane the daemon itself spawned, the
// daemon. EPERM counts as delivered-shaped rather than a failure: the process
// exists and is simply not ours, so reporting "no such process" would be a lie.
func spawnedWorkSignalPID(pid int, force bool) bool {
	if pid <= 1 {
		return false
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	err := syscall.Kill(pid, signal)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// spawnedWorkListeningPIDs reports which of the given processes hold a
// listening TCP socket. It is half of the service verdict: binding a port is
// something servers do and build, test and agent lanes do not, and unlike a
// declaration it needs no cooperation from a process that is already running.
func spawnedWorkListeningPIDs(pids []int) (map[int]bool, bool) {
	result := make(map[int]bool, len(pids))
	if len(pids) == 0 {
		return result, true
	}
	list := make([]string, 0, len(pids))
	for _, pid := range pids {
		if pid > 1 {
			list = append(list, strconv.Itoa(pid))
		}
	}
	if len(list) == 0 {
		return result, true
	}
	command, err := exec.LookPath("lsof")
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	args := []string{"-nP", "-a", "-iTCP", "-sTCP:LISTEN", "-Fp", "-p", strings.Join(list, ",")}
	out, err := exec.CommandContext(ctx, command, args...).Output()
	if err != nil && len(out) == 0 {
		// lsof exits 1 when none of the listed processes hold a matching socket.
		// That is an authoritative "none of these are servers", not a failure —
		// only a timeout leaves us genuinely uninformed.
		if ctx.Err() != nil {
			return nil, false
		}
		return result, true
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "p") {
			continue
		}
		if pid, ok := parseSpawnedWorkPID(strings.TrimPrefix(line, "p")); ok {
			result[pid] = true
		}
	}
	return result, true
}

func spawnedWorkPIDsForOutputs(paths []string) (map[string][]int, bool) {
	result := make(map[string][]int, len(paths))
	if len(paths) == 0 {
		return result, true
	}
	for _, path := range paths {
		result[path] = []int{}
	}
	aliases := make(map[string]string, len(paths)*2)
	for _, path := range paths {
		clean := filepath.Clean(path)
		aliases[clean] = path
		if canonical, err := filepath.EvalSymlinks(clean); err == nil {
			aliases[filepath.Clean(canonical)] = path
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	command, err := exec.LookPath("lsof")
	if err != nil {
		return nil, false
	}
	args := append([]string{"-Fpn", "--"}, paths...)
	out, err := exec.CommandContext(ctx, command, args...).Output()
	if err != nil && len(out) == 0 {
		// lsof exits 1 when no process has the file open; that is a supported,
		// authoritative empty result rather than a probe failure.
		if ctx.Err() != nil {
			return nil, false
		}
		return result, true
	}
	seen := make(map[string]map[int]struct{}, len(paths))
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "p") {
			pid, _ = parseSpawnedWorkPID(strings.TrimPrefix(line, "p"))
			continue
		}
		if !strings.HasPrefix(line, "n") || pid <= 0 {
			continue
		}
		path, ok := aliases[filepath.Clean(strings.TrimPrefix(line, "n"))]
		if !ok {
			continue
		}
		if seen[path] == nil {
			seen[path] = map[int]struct{}{}
		}
		seen[path][pid] = struct{}{}
	}
	for path, pathPIDs := range seen {
		pids := make([]int, 0, len(pathPIDs))
		for pid := range pathPIDs {
			pids = append(pids, pid)
		}
		sort.Ints(pids)
		result[path] = pids
	}
	return result, true
}
