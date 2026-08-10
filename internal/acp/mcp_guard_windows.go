//go:build windows

package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
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
	<-timer.C
	for {
		m.guardAcpMCPFanout("periodic")
		time.Sleep(interval)
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
	ids := make([]string, 0, len(bridges))
	for pid := range bridges {
		ids = append(ids, strconv.Itoa(pid))
	}
	powershell := "powershell.exe"
	if resolved, err := exec.LookPath("pwsh.exe"); err == nil {
		powershell = resolved
	}
	script := fmt.Sprintf(`$ErrorActionPreference='SilentlyContinue'; $ids=@(%s); Get-CimInstance Win32_Process -Filter "name = 'docker.exe'" | Where-Object { $ids -contains [int]$_.ParentProcessId } | Select-Object ProcessId,ParentProcessId,CommandLine | ConvertTo-Json -Compress -Depth 3`, strings.Join(ids, ","))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := managedCommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	var stdout limitedOutputBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &limitedOutputBuffer{}
	if err := cmd.Run(); err != nil && len(bytes.TrimSpace(stdout.Bytes())) == 0 {
		return nil, err
	}
	if stdout.overflow {
		return nil, errors.New("Docker MCP process snapshot exceeded 2 MiB")
	}
	data := bytes.TrimSpace(stdout.Bytes())
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return map[int][]windowsDockerProcess{}, nil
	}
	var list []windowsDockerProcess
	if data[0] == '{' {
		var one windowsDockerProcess
		if err := json.Unmarshal(data, &one); err != nil {
			return nil, err
		}
		list = []windowsDockerProcess{one}
	} else if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	out := make(map[int][]windowsDockerProcess)
	for _, row := range list {
		if row.ProcessID > 0 && bridges[row.ParentProcessID] != nil && isRawMCPDockerCommandLine(row.CommandLine) {
			out[row.ParentProcessID] = append(out[row.ParentProcessID], row)
		}
	}
	return out, nil
}

type limitedOutputBuffer struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *limitedOutputBuffer) Write(p []byte) (int, error) {
	const limit = 2 * 1024 * 1024
	written := len(p)
	remaining := limit - b.buf.Len()
	if remaining <= 0 {
		b.overflow = true
		return written, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.buf.Write(p)
	return written, nil
}

func (b *limitedOutputBuffer) Bytes() []byte { return b.buf.Bytes() }
