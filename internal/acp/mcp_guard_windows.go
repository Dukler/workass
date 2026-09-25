//go:build windows

package acp

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

const defaultMCPFanoutGuardInterval = 30 * time.Second

type windowsDockerProcess struct {
	ProcessID       int    `json:"ProcessId"`
	ParentProcessID int    `json:"ParentProcessId"`
	CommandLine     string `json:"CommandLine"`
}

func (m *Manager) mcpFanoutLoop() {
	if strings.TrimSpace(os.Getenv("WORK_ASSISTANT_MCP_FANOUT_GUARD")) == "0" {
		return
	}
	interval := defaultMCPFanoutGuardInterval
	if raw := strings.TrimSpace(os.Getenv("WORK_ASSISTANT_MCP_FANOUT_GUARD_MS")); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms >= 5000 {
			interval = time.Duration(ms) * time.Millisecond
		}
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-m.loopStop:
		return
	case <-timer.C:
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		m.guardAcpMCPFanout("periodic")
		select {
		case <-m.loopStop:
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) guardAcpMCPFanout(reason string) {
	m.mu.Lock()
	all := make([]*Bridge, 0, len(m.bridges))
	for _, bridge := range m.bridges {
		if bridge != nil {
			all = append(all, bridge)
		}
	}
	m.mu.Unlock()
	bridges := make(map[int]*Bridge)
	for _, bridge := range all {
		if pid := bridge.childPID(); pid > 0 {
			bridges[pid] = bridge
		}
	}
	if len(bridges) == 0 {
		return
	}
	rows, err := rawMCPDockerChildren(bridges)
	if err != nil {
		m.opts.Logf("raw MCP fanout guard failed", map[string]any{"reason": reason, "error": redactSensitiveText(err.Error())})
		return
	}
	for parentPID, children := range rows {
		bridge := bridges[parentPID]
		if bridge == nil || len(children) == 0 {
			continue
		}
		blocked := make([]int, 0, len(children))
		commands := make([]string, 0, len(children))
		for _, child := range children {
			commands = append(commands, compactText(redactSensitiveText(child.CommandLine), 500))
			blocked = append(blocked, child.ProcessID)
		}
		m.opts.Logf("blocked raw MCP docker fanout", map[string]any{
			"reason": reason, "bridgeKey": bridge.Key(), "enginePid": parentPID,
			"count": len(children), "blocked": blocked, "commands": commands,
		})
		bridge.Close(true, errors.New("raw Docker MCP fan-out blocked; Workass requires shared MCP proxy config"))
	}
}

func rawMCPDockerChildren(bridges map[int]*Bridge) (map[int][]windowsDockerProcess, error) {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(snapshot)
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	out := make(map[int][]windowsDockerProcess)
	for err = syscall.Process32First(snapshot, &entry); err == nil; err = syscall.Process32Next(snapshot, &entry) {
		parent := int(entry.ParentProcessID)
		if bridges[parent] == nil || !strings.EqualFold(syscall.UTF16ToString(entry.ExeFile[:]), "docker.exe") {
			continue
		}
		command, commandErr := windowsProcessCommandLine(entry.ProcessID)
		if commandErr != nil {
			continue // exited between snapshot and query
		}
		row := windowsDockerProcess{ProcessID: int(entry.ProcessID), ParentProcessID: parent, CommandLine: command}
		if row.ProcessID > 0 && isRawMCPDockerCommandLine(row.CommandLine) {
			out[parent] = append(out[parent], row)
		}
	}
	if !errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return out, nil
}

var procNtQueryInformationProcess = syscall.NewLazyDLL("ntdll.dll").NewProc("NtQueryInformationProcess")

const (
	processCommandLineInformation = 60 // PROCESSINFOCLASS, Windows 8.1+
	statusInfoLengthMismatch      = 0xC0000004
	maxProcessCommandLineBytes    = 1 << 20
)

type unicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

// windowsProcessCommandLine reads another process's command line with
// PROCESS_QUERY_LIMITED_INFORMATION only; it never reads process memory.
func windowsProcessCommandLine(pid uint32) (string, error) {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(handle)
	size := uint32(4096)
	for size <= maxProcessCommandLineBytes {
		buffer := make([]byte, size)
		var needed uint32
		status, _, _ := procNtQueryInformationProcess.Call(uintptr(handle), processCommandLineInformation,
			uintptr(unsafe.Pointer(&buffer[0])), uintptr(size), uintptr(unsafe.Pointer(&needed)))
		if uint32(status) == statusInfoLengthMismatch {
			if needed <= size {
				needed = size * 2
			}
			size = needed
			continue
		}
		if status != 0 {
			return "", fmt.Errorf("NtQueryInformationProcess status 0x%08x", uint32(status))
		}
		value := (*unicodeString)(unsafe.Pointer(&buffer[0]))
		if value.Buffer == nil || value.Length == 0 {
			return "", nil
		}
		units := unsafe.Slice(value.Buffer, int(value.Length)/2)
		return string(utf16.Decode(units)), nil
	}
	return "", errors.New("process command line exceeds 1 MiB")
}
