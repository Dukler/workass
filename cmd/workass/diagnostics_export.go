package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/wire"
)

// diagnostics:export (Ajustes · Mantenimiento · «Exportar diagnóstico») is one
// report a person can save and attach when something goes wrong: what this
// daemon is, how its agents' processes are doing, which agents it found, how
// much disk it uses, and the end of each log file Workass writes. It is a read
// and changes nothing.
//
// Everything in it is redacted on the way out: a field whose name says it holds
// a secret is replaced, and every string goes through the same secret-shaped
// text redaction as logs and UI payloads (acp.RedactSensitiveText).

const (
	diagnosticsSchema       = "workass-diagnostics/1"
	diagnosticsLogTailBytes = 256 << 10
	diagnosticsLogFiles     = 6
	diagnosticsStorageWait  = 10 * time.Second
	// logRootEnvVar is where the shell writes this daemon's stdout and stderr
	// (runtime-bootstrap.js) and where a test root keeps its logs.
	logRootEnvVar = "WORKASS_LOG_ROOT"
)

type diagnosticsReport struct {
	state     *daemonState
	manager   *acp.Manager
	storage   *storageUsageMeter
	logRoot   string
	startedAt time.Time
	now       func() time.Time
}

// daemonStartedAt is when this daemon process started.
var daemonStartedAt = time.Now()

func (d diagnosticsReport) build() map[string]any {
	return d.buildWith(diagnosticsLogFiles, diagnosticsLogTailBytes)
}

// buildWith is build with its own bound on how much of each log it keeps.
func (d diagnosticsReport) buildWith(logFiles int, logTailBytes int64) map[string]any {
	now := d.now()
	report := map[string]any{
		"schema":      diagnosticsSchema,
		"generatedAt": now.UTC().Format(time.RFC3339),
		"redaction":   "Secret-named fields and secret-shaped text are replaced with [redacted].",
		"daemon": map[string]any{
			"version":       daemonVersion,
			"os":            runtime.GOOS,
			"arch":          runtime.GOARCH,
			"go":            runtime.Version(),
			"profile":       workassRuntimeProfile(),
			"pid":           os.Getpid(),
			"uptimeSeconds": int64(now.Sub(d.startedAt).Seconds()),
		},
	}
	if d.state != nil && d.state.config != nil {
		report["engine"] = d.state.config.effectiveEngine().publicMap()
	}
	if d.manager != nil {
		report["providers"] = pickEach(d.manager.ProvidersList(), "id", "name", "enabled", "status", "error", "needsLogin",
			"disabledByUser", "detected", "detectedAt", "resolvedCommand", "cliVersion", "fixHint")
		report["processes"] = pickEach(d.manager.Processes(), "id", "kind", "label", "pid", "status", "state", "engine",
			"managed", "rssKb", "startedAt", "finishedAt", "code", "command")
	}
	if d.storage != nil {
		if snapshot, ok, err := d.storage.read(diagnosticsStorageWait); err != nil {
			report["storage"] = map[string]any{"error": err.Error()}
		} else {
			report["storage"] = storageUsageReply(snapshot, ok)
		}
	}
	logs, note := diagnosticsLogTailsLimited(d.logRoot, logFiles, logTailBytes)
	report["logs"] = logs
	if note != "" {
		report["logsNote"] = note
	}
	redacted, _ := redactDiagnostics(report).(map[string]any)
	return redacted
}

// pickEach keeps only the named fields of each row.
func pickEach(rows []map[string]any, keys ...string) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		item := make(map[string]any, len(keys))
		for _, key := range keys {
			if value, ok := row[key]; ok {
				item[key] = value
			}
		}
		out = append(out, item)
	}
	return out
}

// diagnosticsLogTails reads the end of the newest log files directly in
// root: at most diagnosticsLogFiles files, diagnosticsLogTailBytes of each,
// starting at a whole line.
func diagnosticsLogTails(root string) ([]any, string) {
	return diagnosticsLogTailsLimited(root, diagnosticsLogFiles, diagnosticsLogTailBytes)
}

func diagnosticsLogTailsLimited(root string, maxFiles int, tailBytes int64) ([]any, string) {
	root = strings.TrimSpace(root)
	if root == "" {
		return []any{}, "This daemon was not told where its logs are."
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return []any{}, "The log folder could not be read."
	}
	type logFile struct {
		name string
		info os.FileInfo
	}
	var files []logFile
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, logFile{name: entry.Name(), info: info})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].info.ModTime().Equal(files[j].info.ModTime()) {
			return files[i].info.ModTime().After(files[j].info.ModTime())
		}
		return files[i].name < files[j].name
	})
	if len(files) > maxFiles {
		files = files[:maxFiles]
	}
	out := make([]any, 0, len(files))
	for _, file := range files {
		tail, err := readLogTail(filepath.Join(root, file.name), tailBytes)
		item := map[string]any{
			"name":       file.name,
			"bytes":      file.info.Size(),
			"modifiedAt": file.info.ModTime().UTC().Format(time.RFC3339),
		}
		if err != nil {
			item["error"] = "could not be read"
		} else {
			item["tail"] = tail
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return out, "The log folder has no log files."
	}
	return out, ""
}

// readLogTail returns the last limit bytes of path, from the first whole line.
func readLogTail(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if info.Size() > limit {
		start = info.Size() - limit
	}
	data, err := io.ReadAll(io.NewSectionReader(file, start, info.Size()-start))
	if err != nil {
		return "", err
	}
	if start > 0 {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	return strings.ToValidUTF8(string(data), "�"), nil
}

// redactDiagnostics redacts secret-named fields and secret-shaped text at
// every depth of the report.
func redactDiagnostics(value any) any {
	switch x := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, item := range x {
			if secretKeyRE.MatchString(key) {
				out[key] = "[redacted]"
				continue
			}
			out[key] = redactDiagnostics(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for index, item := range x {
			out[index] = redactDiagnostics(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for index, item := range x {
			out[index] = redactDiagnostics(item)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for index, item := range x {
			out[index] = acp.RedactSensitiveText(item)
		}
		return out
	case string:
		return acp.RedactSensitiveText(x)
	default:
		return value
	}
}

// registerMaintenanceHandlers installs the Mantenimiento reads. Both may take
// seconds (a walk of the state directory), so neither waits behind the
// ordered lane nor holds it up.
func registerMaintenanceHandlers(hub *wire.Hub, state *daemonState, manager *acp.Manager) int {
	stateDir := ""
	if state != nil {
		stateDir = state.stateDir
	}
	meter := newStorageUsageMeter(stateDir)
	hub.RegisterConcurrentRead("storage:usage", func(args []any) (any, error) {
		snapshot, ok, err := meter.read(storageUsageWait)
		if err != nil {
			return nil, err
		}
		return storageUsageReply(snapshot, ok), nil
	})
	startedAt := time.Now()
	hub.RegisterConcurrentRead("diagnostics:export", func(args []any) (any, error) {
		return diagnosticsReport{
			state: state, manager: manager, storage: meter,
			logRoot: os.Getenv(logRootEnvVar), startedAt: startedAt, now: time.Now,
		}.build(), nil
	})
	return 2
}
