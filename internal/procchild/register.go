package procchild

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"workass/internal/toolcli"
)

// The one place that speaks to the daemon.
//
// Registration is a tool call like any other — `workass_register_process_child`
// over the pinned loopback HTTPS the tools client already uses — and it carries
// the caller's own operation id, so the daemon's answer to a repeat is the row
// it already has. Three consequences, all of them the point:
//
//   - A retry after an uncertain reply cannot create a second child. §6 calls
//     this "one operation_id for both sides"; the failure it removes is the one
//     where "launch there" succeeded, "record here" was retried, and the chat
//     ended up with two rows for one job or one row for a job that had moved on.
//   - The daemon can be DOWN at this moment without the run being affected. The
//     spool is the record; registration is how the chat learns promptly.
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

// daemonRegistrar registers through the ordinary tool endpoint.
type daemonRegistrar struct {
	config toolcli.Config
	// consume is the path to unlink after a successful read, set only for a
	// token file minted for this run — never for the engine's own context,
	// which belongs to the chat and outlives this job.
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
func ResolveCredential(tokenFile string) (Registrar, string, error) {
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
	registrar := &daemonRegistrar{config: config, timeout: 20 * time.Second}
	if consume {
		registrar.consume = path
	}
	return registrar, path, nil
}

// Register performs one registration call.
func (d *daemonRegistrar) Register(
	spec Spec, childID, outputFile, doneFile, spoolFile string, supervisorPID, pgid int, group GroupKind,
) (Registration, error) {
	if d == nil {
		return Registration{}, errors.New("this run has no Workass credential")
	}
	arguments := map[string]any{
		"operation_id":  strings.TrimSpace(spec.OperationID),
		"work_id":       childID,
		"label":         spec.Label,
		"role":          strings.TrimSpace(spec.Role),
		"output_file":   outputFile,
		"done_file":     doneFile,
		"spool_file":    spoolFile,
		"pid":           supervisorPID,
		"process_group": pgid,
		"isolation":     string(group),
		"command":       strings.Join(spec.Command, " "),
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
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	response, err := toolcli.Do(ctx, d.config, "", &toolcli.Call{
		Name: "workass_register_process_child", Arguments: arguments,
	})
	if err != nil {
		return Registration{}, err
	}
	if strings.TrimSpace(response.Error) != "" {
		return Registration{}, errors.New(response.Error)
	}
	if d.consume != "" {
		// A credential minted for this run has been used; leaving it on disk
		// for the length of a training job is a window with no purpose.
		_ = os.Remove(d.consume)
		d.consume = ""
	}
	fields, _ := response.Result.(map[string]any)
	registration := Registration{ChildID: childID, OutputFile: outputFile, DoneFile: doneFile}
	if value, ok := fields["workId"].(string); ok && strings.TrimSpace(value) != "" {
		registration.ChildID = value
	}
	if value, ok := fields["status"].(string); ok {
		registration.Status = value
	}
	return registration, nil
}

// StateDirForCredential derives the daemon's state directory from the
// credential's own path, which is the one place it is reliably knowable
// without a new environment variable.
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
