package procchild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"workass/internal/toolcli"
)

// The one place that speaks to the daemon.
//
// A report is a tool call like any other — `workass_register_process_child`
// over the pinned loopback HTTPS the tools client already uses — carrying the
// caller's own operation id and the batch of events not yet acknowledged. The
// daemon applies them in seq order and answers with how far it got. Four
// consequences, all of them the point:
//
//   - A retry after an uncertain reply cannot create a second child, and
//     cannot apply an event twice. §6 calls this "one operation_id for both
//     sides"; the daemon dedups events by seq on top of it.
//   - This is the PRIMARY path. The chat learns from the report, not from
//     anything on disk, which is the whole correction the first live test
//     forced: four jobs wrote a perfect spool and no chat was ever told.
//   - The daemon can still be DOWN at this moment without the run being
//     affected. The outbox holds the batch and the next report delivers it.
//   - The credential never touches argv. A command line is readable by every
//     process on the box through `ps` and lands in shell history, which is why
//     the fleet key has never been accepted there either
//     (cmd/workass/fleet_cli.go). The token comes from a file whose path is an
//     argument, or from the environment.

// TokenEnvVar names a file holding the supervisor's credential. It is accepted
// in addition to WORKASS_TOOL_CONTEXT so a lane runner can hand one job its own
// short-lived credential without touching the chat engine's.
const TokenEnvVar = "WORKASS_RUN_TOKEN_FILE"

// ContextEnvVar is the credential every Workass-launched process already has.
const ContextEnvVar = "WORKASS_TOOL_CONTEXT"

// DataRootEnvVar is set by the launchd/systemd unit and by every development
// profile, so a shell that has sourced one already knows where this machine's
// daemon keeps its state.
const DataRootEnvVar = "WORKASS_DATA_ROOT"

// daemonReporter reports through the ordinary tool endpoint.
type daemonReporter struct {
	config toolcli.Config
	// consume is the path to unlink after a successful read, set only for a
	// token file minted for this run — never for the engine's own context,
	// which belongs to the chat and outlives this job.
	//
	// It is unlinked as soon as it has been READ, not when the job ends: the
	// credential is in this process's memory from then on and the job reports
	// with it for its whole life, so leaving the file on disk for the length of
	// a training run is a window with no purpose.
	consume string
	timeout time.Duration
}

// ResolveCredential finds the credential for one run and reports where the
// daemon's state lives, which is also derivable from it.
//
// Order: an explicit --token-file, then WORKASS_RUN_TOKEN_FILE, then the
// WORKASS_TOOL_CONTEXT every Workass-launched process already holds. A run with
// none of them still works — it supervises, spools and settles — it simply has
// no chat to tell, and says so.
func ResolveCredential(tokenFile string) (Reporter, string, error) {
	path, consume := strings.TrimSpace(tokenFile), true
	if path == "" {
		path, consume = strings.TrimSpace(os.Getenv(TokenEnvVar)), true
	}
	if path == "" {
		path, consume = strings.TrimSpace(os.Getenv(ContextEnvVar)), false
	}
	if path == "" {
		return nil, "", nil
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
			// A credential in a world-readable file is a credential every
			// process on this machine holds. Refusing is the only honest
			// answer; widening the rule to "it is probably fine" is how a
			// token ends up in a tarball.
			return nil, "", fmt.Errorf("%s is readable by other users; a Workass credential file must be mode 0600", path)
		}
	}
	config, err := toolcli.ReadConfig(path)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(config.Credential) == "" {
		return nil, "", errors.New("that Workass credential file carries no credential")
	}
	reporter := &daemonReporter{config: config, timeout: 20 * time.Second}
	if consume {
		// Read, and therefore spent: the token lives in this process from here
		// on, and the file it came from has no further purpose.
		_ = os.Remove(path)
	}
	return reporter, path, nil
}

// CredentialStateDir is the daemon state directory a credential file names.
//
// It is the authoritative answer to "where does this machine's daemon keep its
// state", because the daemon itself wrote it there. The alternative — deriving
// it, or falling back to a default — is what produced the bug this redesign
// started from: a relative `state` resolved against the JOB's working
// directory, so four jobs reported into /tmp/state where no daemon would look.
func CredentialStateDir(tokenFile string) string {
	registrarPath := strings.TrimSpace(tokenFile)
	if registrarPath == "" {
		return ""
	}
	config, err := toolcli.ReadConfig(registrarPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(config.StateDir)
}

// Report performs one report: the registration facts and every event the
// daemon has not acknowledged, in seq order.
func (d *daemonReporter) Report(request Report) (Acknowledgement, error) {
	if d == nil {
		return Acknowledgement{}, errors.New("this run has no Workass credential")
	}
	spec := request.Spec
	arguments := map[string]any{
		"operation_id":  strings.TrimSpace(spec.OperationID),
		"work_id":       request.ChildID,
		"label":         spec.Label,
		"role":          strings.TrimSpace(spec.Role),
		"output_file":   request.OutputFile,
		"done_file":     request.DoneFile,
		"pid":           request.SupervisorPID,
		"process_group": request.ProcessGroup,
		"isolation":     string(request.Group),
		"command":       strings.Join(spec.Command, " "),
		"events":        eventArguments(request.Events),
	}
	if resource := NormalizeResource(spec.Resource); resource != "" {
		arguments["resource"] = resource
	}
	if spec.MaxRuntime > 0 {
		arguments["max_runtime_ms"] = spec.MaxRuntime.Milliseconds()
	}
	if spec.StaleAfter > 0 {
		arguments["stale_after_ms"] = spec.StaleAfter.Milliseconds()
	}
	if spec.MaxHold > 0 {
		arguments["max_hold_ms"] = spec.MaxHold.Milliseconds()
	}
	if owner := strings.TrimSpace(spec.Owner); owner != "" {
		arguments["owner"] = owner
	}
	if request.Heartbeat > 0 {
		arguments["heartbeat_ms"] = request.Heartbeat.Milliseconds()
	}
	if request.ReportGap > 0 {
		// Told rather than inferred: the daemon could only guess at an outage
		// that is already over, and a row that silently healed would hide that
		// the job spent twenty minutes unable to say anything.
		arguments["report_gap_ms"] = request.ReportGap.Milliseconds()
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	response, err := toolcli.Do(ctx, d.config, "", &toolcli.Call{
		Name: "workass_register_process_child", Arguments: arguments,
	})
	if err != nil {
		return Acknowledgement{}, err
	}
	if strings.TrimSpace(response.Error) != "" {
		return Acknowledgement{}, errors.New(response.Error)
	}
	fields, _ := response.Result.(map[string]any)
	ack := Acknowledgement{ChildID: request.ChildID}
	if value, ok := fields["workId"].(string); ok && strings.TrimSpace(value) != "" {
		ack.ChildID = value
	}
	if value, ok := fields["status"].(string); ok {
		ack.Status = value
	}
	ack.AckThrough = ackThroughOf(fields["ackThrough"])
	// Whether the ack is sufficient is the journal's call, not this client's:
	// one authority for "was this delivered", and it is the one that owns the
	// queue (reporter.go).
	return ack, nil
}

// ackThroughOf reads the acknowledgement out of the daemon's answer, in
// whatever shape it arrives in.
//
// The tools endpoint decodes to float64 today, so this is tolerance rather
// than a fix — but the cost of getting it wrong is not a wrong number, it is a
// supervisor that reads 0, concludes its daemon is too old, and redelivers the
// same batch forever while the daemon applies it every time. The same trap
// already cost every exit code of every job reported over a link, where the
// wire encodes numbers as strings, so it is worth refusing to fall into twice.
func ackThroughOf(value any) uint64 {
	switch typed := value.(type) {
	case float64:
		if typed > 0 {
			return uint64(typed)
		}
	case int64:
		if typed > 0 {
			return uint64(typed)
		}
	case int:
		if typed > 0 {
			return uint64(typed)
		}
	case json.Number:
		if parsed, err := typed.Int64(); err == nil && parsed > 0 {
			return uint64(parsed)
		}
	case string:
		if parsed, err := strconv.ParseUint(strings.TrimSpace(typed), 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

// eventArguments is the wire shape of one batch. It is the Event struct's own
// JSON, built explicitly so a change to the struct cannot quietly change the
// wire.
func eventArguments(events []Event) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, event := range events {
		entry := map[string]any{
			"seq": event.Seq, "kind": event.Kind,
			"at": event.At.UTC().Format(time.RFC3339Nano),
		}
		if len(event.Payload) > 0 {
			entry["payload"] = event.Payload
		}
		out = append(out, entry)
	}
	return out
}

// StateDirForCredential derives the daemon's state directory from the
// credential's own PATH. It is the fallback behind CredentialStateDir, for a
// context file written by a build that predates the stated field.
//
// The engine writes its context at
// <state>/tool-bootstrap/bridge-XXXX/context.json (internal/acp's
// native_instructions.go), so the state directory is three levels up. This is
// read-only inference from a path the daemon itself chose — no new contract,
// and nothing to keep in sync.
func StateDirForCredential(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	bridge := parentDir(path)
	bootstrap := parentDir(bridge)
	if baseName(bootstrap) != "tool-bootstrap" {
		return ""
	}
	return parentDir(bootstrap)
}

func parentDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Dir(path)
}

func baseName(path string) string { return filepath.Base(strings.TrimSpace(path)) }
