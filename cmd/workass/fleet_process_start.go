package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/fleetid"
	"workass/internal/procchild"
	"workass/internal/toolcli"
)

// Starting a supervised job on the machine that has the files.
//
// This closes the gap the first live P1–P4 test found and the operator named:
// `workass run --owner wa:<P>/chat/<c>` already lets a job on machine H report
// into a chat on machine P, and registration is deliberately local — the pid is
// the evidence, and only H can probe it (docs/specs/fleet-runtime.md §13 P4).
// But nothing could START that job from P. `workass run` in a plain shell on H
// says so plainly ("no Workass credential in this environment ... but no chat is
// told"), and an agent on P had no option except asking an agent that happened
// to live on H.
//
// ## The spec tension, stated rather than hidden
//
// §9 and §15 keep "daemon-side spawning" a non-goal. The reason given there is
// the one that matters — "so the daemon never lends its environment to a
// child" — and this path keeps that reason intact:
//
//   - the job's environment is BUILT, not inherited: PATH, HOME, the data root,
//     its own credential path, plus the variables the caller stated by name.
//     Nothing of the daemon's own environment reaches it, so no provider
//     credential, token or session path can;
//   - the only credential it holds is a one-shot run token (acp/run_tokens.go)
//     good for exactly one `process.register` with one operation id;
//   - it is still `workass run` that supervises, and that supervisor moves itself
//     out of the daemon's reach (§14g), so the job survives a restart or a
//     self-update and the answer below says whether it really did;
//   - it is refused outright on a link below full access, and refused for this
//     machine, where the agent has its own shell and should use it.
//
// The literal non-goal is nonetheless amended by this path, which is an
// operator decision (asked for on 2026-10-30) and is recorded as such. Nothing
// here spawns anything the operator's own fleet did not ask for.

// processStartEnvAllowed bounds what may be named in the job's environment. A
// variable whose NAME or VALUE looks like a secret is refused rather than
// forwarded: a secret belongs in a file on the machine that runs the job.
var processStartSecretMarkers = []string{"api_key", "apikey", "token", "secret", "password", "credential", "bearer"}

// maxProcessStartCommand bounds one job's argv.
const maxProcessStartCommand = 64

// processStartLocalReason is why this machine refuses to start a job for an
// agent that is already standing on it.
const processStartLocalReason = "a supervised job on THIS machine is started by `workass run` in your own shell, which is what keeps the daemon out of your environment (fleet-runtime §9); pass machine_id to ask another fleet machine to run one"

// startProcessChildHere serves `process.start` on the machine that will run the
// job. It returns as soon as the supervisor is launched: the job's outcome
// arrives in the owning chat as a notice, through the monitor the supervisor's
// own registration offers (§7).
func (h *agentControlHandler) startProcessChildHere(ctx context.Context, forwarded fleetForwardedWork, params map[string]any) (any, error) {
	if h == nil || h.manager == nil {
		return nil, errors.New("supervised jobs require the chat manager")
	}
	// §11: narrowed work may not run arbitrary commands here. The refusal names
	// the capability, as every structural refusal must.
	if canonicalFleetLimit(forwarded.Limit) != fleetLimitFull {
		return nil, fleetNarrowedCapabilityError("process.start", "process.start", canonicalFleetLimit(forwarded.Limit))
	}
	if forwarded.Origin.empty() || strings.TrimSpace(forwarded.Origin.ChatID) == "" {
		return nil, errors.New("a supervised job started from another machine needs the chat that asked, so its outcome has somewhere to go")
	}
	operationID := strings.TrimSpace(firstNonEmptyString(fieldString(params, "operation_id"), forwarded.OperationID))
	if operationID == "" {
		return nil, errors.New("process.start requires a caller-stable operation_id; one id is one job, never a second one")
	}
	childID, err := procchild.IDForOperation(operationID)
	if err != nil {
		return nil, err
	}

	label := strings.TrimSpace(fieldString(params, "label"))
	if label == "" {
		return nil, errors.New("a supervised job needs a label: it is the only name its chat will ever show")
	}
	command, err := processStartCommand(params)
	if err != nil {
		return nil, err
	}
	cwd := strings.TrimSpace(fieldString(params, "cwd"))
	if !filepath.IsAbs(cwd) {
		return nil, errors.New("process.start requires an absolute cwd on this machine; a path from the calling machine would miss, or hit a different directory")
	}
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		return nil, fmt.Errorf("process.start cwd is not a readable directory on this machine: %s", cwd)
	}
	environment, err := processStartEnvironment(params)
	if err != nil {
		return nil, err
	}

	owner, err := fleetid.New(forwarded.Origin.Machine, fleetid.Chat, forwarded.Origin.ChatID)
	if err != nil {
		return nil, fmt.Errorf("the asking chat is not a resolvable fleet id: %w", err)
	}
	// One operation id is one job. A replay that arrives after an uncertain
	// reply answers with the job that exists, rather than starting a second
	// supervisor over the first one's id, output file and stop file.
	if existing, found := h.manager.ProcessChildByID(childID); found {
		if strings.TrimSpace(existing.Owner) != owner.String() {
			return nil, fmt.Errorf("operation_id %q already names a job owned by %s; one id is one job, so choose another", operationID, existing.Owner)
		}
		return map[string]any{
			"ok": true, "replayed": true, "workId": childID, "operationId": operationID,
			"machineId": h.selfMachineID, "owner": owner.String(), "status": existing.Status,
			"outputFile": existing.OutputFile,
			"queuedAt":   existing.QueuedAt, "startedAt": existing.StartedAt,
		}, nil
	}
	executable, err := procchild.SupervisorExecutable(h.manager.StateDir())
	if err != nil {
		return nil, fmt.Errorf("this daemon cannot locate its own executable, so it cannot start `workass run`: %w", err)
	}
	credentialPath, err := h.writeRunCredential(childID, owner.String(), operationID)
	if err != nil {
		return nil, err
	}
	// The output path is resolved HERE, by the same function the supervisor
	// uses, because this daemon registers the row before the first report
	// arrives and the row must name the file that is actually being written.
	outputFile, err := procchild.OutputPath(h.manager.StateDir(), childID, fieldString(params, "output_file"))
	if err != nil {
		return nil, err
	}
	logPath := procchild.SupervisorLogPath(outputFile)
	log, err := openSupervisorLog(logPath, childID, label)
	if err != nil {
		return nil, err
	}

	arguments := processStartArguments(label, owner.String(), operationID, cwd, credentialPath, h.manager.StateDir(), outputFile, params, command)
	cmd := exec.Command(executable, arguments...)
	cmd.Dir = cwd
	cmd.Env = environment
	cmd.Stdin = nil
	// Captured, not discarded. The supervisor's stderr is where "NOT
	// REPORTING" is said and its stdout is the closing receipt, and both went
	// to /dev/null here: the one sentence that would have named the live
	// failure was written by a process with nowhere to write it. The file sits
	// next to the job's own output, so a reader who has one has the other.
	cmd.Stdout, cmd.Stderr = log, log
	// Its own process group, which is all that launchd and the app's updater reach
	// for. Under a systemd service that is not enough: a group does not escape a
	// cgroup, so the supervisor moves itself into a scope as it starts, keeping
	// this pid (procchild.EscapeServiceCgroup).
	procchild.DetachProcessGroup(cmd)
	startErr := cmd.Start()
	// The child holds its own descriptor from here; this one has no further
	// use, and keeping it would hold the file open for the length of the job.
	_ = log.Close()
	if startErr != nil {
		return nil, fmt.Errorf("could not start `workass run` on this machine: %s", acp.RedactSensitiveText(startErr.Error()))
	}
	supervisorPID := cmd.Process.Pid
	// Reaped off-thread so this daemon leaves no zombie behind; the job's
	// outcome never comes from here, it comes from the spool the supervisor
	// writes and the registration it makes.
	go func() { _ = cmd.Wait() }()

	// The row, written now rather than waited for. A supervisor whose every
	// report is refused used to leave NOTHING behind — no row, so no silence
	// notice, no `lost` verdict and no outbox drain, because every one of those
	// hangs off a row — and the job looked, from the owner's side, like a job
	// that had never been asked for. Registering here makes the launch itself
	// the record: if the supervisor reports, its first report amends this row
	// (same work id, §6); if it never does, the row goes `silent` within a beat
	// and says where to read its complaint.
	reporting := "pending"
	registration, regErr := h.registerStartedProcessChild(startedProcessChild{
		ChildID: childID, Label: label, Owner: owner.String(), SupervisorPID: supervisorPID, Command: command,
		OutputFile: outputFile, SupervisorLog: logPath, Params: params,
	})
	if regErr != nil {
		// Not a failed start: the job is running. It is a job this machine
		// cannot promise to tell anybody about, which is exactly the thing that
		// must never be reported as fine.
		reporting = "unregistered: " + acp.RedactSensitiveText(regErr.Error())
	}

	result := map[string]any{
		"ok": true, "workId": childID, "operationId": operationID,
		"machineId": h.selfMachineID, "supervisorPid": supervisorPID,
		"owner": owner.String(), "outputFile": outputFile, "supervisorLog": logPath,
		"reporting": reporting,
		// Not "startedAt": the job's command has not started, and may wait hours
		// for a resource before it does. workass_list_process_children says when
		// it has.
		"queuedAt": time.Now().UTC().Format(time.RFC3339),
	}
	if monitor, ok := registration["monitor"]; ok {
		result["monitor"] = monitor
	}
	// Measured, not assumed. A job this machine cannot protect still runs, because
	// the caller asked for it, and the caller is told here rather than finding out
	// from an update.
	survival := procchild.AwaitSurvival(supervisorPID, processStartSurvivalWait)
	if !procchild.PIDAlive(supervisorPID) {
		// Already over, as a refused job is at once. Nothing is left to protect, and
		// its outcome comes through its monitor.
		return result, nil
	}
	result["survivesRestart"] = survival.Protected
	if survival.Protected {
		result["survival"] = survival.How
	} else {
		result["warning"] = "NOT ISOLATED: this job will die if this machine's Workass daemon stops or updates: " + survival.Why
	}
	return result, nil
}

// processStartSurvivalWait covers the moment a supervisor takes to move itself.
const processStartSurvivalWait = 3 * time.Second

// openSupervisorLog creates the file the supervisor's own stream goes into and
// stamps what it is, so a reader who opens it mid-job knows whose it is.
func openSupervisorLog(path, childID, label string) (*os.File, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("this machine could not decide where to capture the supervisor's own output")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("this machine could not open %s to capture the supervisor's own output: %s", path, acp.RedactSensitiveText(err.Error()))
	}
	fmt.Fprintf(file, "# workass run: supervisor for %s (%s), started %s\n",
		childID, label, time.Now().UTC().Format(time.RFC3339))
	return file, nil
}

// startedProcessChild is one launch, as the row this daemon writes for it.
type startedProcessChild struct {
	ChildID       string
	Label         string
	Owner         string
	SupervisorPID int
	Command       []string
	OutputFile    string
	SupervisorLog string
	Params        map[string]any
}

// registerStartedProcessChild writes the launch's row on this machine and offers
// the owner its monitor, through the same registration a report uses.
//
// The same one deliberately: a second write path for "the daemon started it"
// would be a second shape of row, and the first report amending this one is
// what keeps a launch and its reports one job rather than two.
func (h *agentControlHandler) registerStartedProcessChild(started startedProcessChild) (map[string]any, error) {
	params := map[string]any{
		"work_id": started.ChildID, "label": started.Label, "owner": started.Owner,
		"pid": started.SupervisorPID, "output_file": started.OutputFile,
		"done_file": started.OutputFile + ".done",
		"command":   strings.Join(started.Command, " "),
		// No isolation: the supervisor decides how it separates the child from
		// itself (a systemd scope, a process group, a job object) and it is the
		// only side that knows which it got. Its first report states it.
		// The cadence the supervisor WILL report at, declared by the side that
		// knows it, so the silence clock is running from the first second.
		"heartbeat_ms": procchild.HeartbeatInterval.Milliseconds(),
	}
	for _, field := range []string{"role", "resource"} {
		if value := strings.TrimSpace(fieldString(started.Params, field)); value != "" {
			params[field] = value
		}
	}
	for _, field := range []string{"max_runtime_ms", "stale_after_ms", "max_hold_ms"} {
		if value := intField(started.Params, field); value > 0 {
			params[field] = value
		}
	}
	value, err := h.registerProcessChildWith("", "", "", params,
		processChildLocalStart{SupervisorLog: started.SupervisorLog, Awaiting: true, Hosted: true})
	if err != nil {
		return nil, err
	}
	return mapFromAnyMain(value), nil
}

// writeRunCredential puts one job's one-shot credential where only that job can
// read it. `workass run` unlinks a --token-file it consumed, and the token
// itself is revoked by the registration it authorized, so the file and the
// grant both end with the job's first report.
func (h *agentControlHandler) writeRunCredential(childID, owner, operationID string) (string, error) {
	template, err := h.manager.ToolContextTemplate()
	if err != nil {
		return "", err
	}
	token := h.manager.MintRunToken(childID, owner, operationID)
	if token == "" {
		return "", errors.New("this daemon could not mint a credential for the job")
	}
	template.Credential = token
	// Where this daemon keeps its state, stated rather than left to be
	// derived. The supervisor resolved it from a default before, and that
	// default resolved `state` against the JOB's working directory: four jobs
	// started with cwd=/tmp wrote their whole lifecycle to /tmp/state and no
	// chat was ever told. The daemon is the one authority on this answer.
	template.StateDir = h.manager.StateDir()
	dir := filepath.Join(h.manager.StateDir(), "run-credentials", childID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "context.json")
	if err := toolcli.WriteConfig(path, template); err != nil {
		return "", err
	}
	return path, nil
}

// processStartCommand reads the job's argv. There is no shell anywhere in this
// path: the program and each argument are separate strings, so nothing is
// quoted, globbed, piped or redirected on the way.
func processStartCommand(params map[string]any) ([]string, error) {
	raw := anySlice(params["command"])
	if len(raw) == 0 {
		return nil, errors.New("process.start needs a command: the program first, then each argument as its own item")
	}
	if len(raw) > maxProcessStartCommand {
		return nil, fmt.Errorf("process.start accepts at most %d command items", maxProcessStartCommand)
	}
	command := make([]string, 0, len(raw))
	for index, value := range raw {
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("every process.start command item must be a string")
		}
		if strings.ContainsRune(text, 0) {
			return nil, errors.New("a process.start command item may not contain a NUL byte")
		}
		if index == 0 && strings.TrimSpace(text) == "" {
			return nil, errors.New("the first process.start command item is the program, and it is empty")
		}
		command = append(command, text)
	}
	return command, nil
}

// processStartArguments is the supervisor's own command line.
//
// Two things in it are the whole point. `--owner` is the chat that ASKED, as a
// global fleet id, which is what makes a job on the box with the files report
// into the conversation that wanted it. And the credential appears only as a
// PATH: a command line is readable by every process on the machine through ps
// and lands in shell history, which is why no Workass credential has ever been
// accepted in argv.
func processStartArguments(label, owner, operationID, cwd, credentialPath, stateDir, outputFile string, params map[string]any, command []string) []string {
	// NOT --quiet. Quiet discards the supervisor's diagnostics entirely, which
	// is a reasonable thing for a caller reading only the receipt to ask for
	// and the wrong thing for the one caller that cannot read anything: this
	// daemon captures the stream to a file nobody is watching, so the only
	// cost of the loud version is a few lines on disk and the only cost of the
	// quiet one was a silent failure.
	arguments := []string{"run", "--label", label, "--owner", owner,
		"--operation-id", operationID, "--cwd", cwd, "--token-file", credentialPath,
		// Belt and braces, and deliberately: the credential states it too, but
		// a job must not be one unreadable file away from reporting into a
		// directory nothing reads.
		"--state-dir", stateDir,
		// Resolved by the daemon so the row it just wrote and the file the
		// supervisor writes are the same path, never two defaults that agree
		// by luck.
		"--output-file", outputFile}
	arguments = append(arguments, processStartDeclarations(params)...)
	arguments = append(arguments, "--")
	return append(arguments, command...)
}

// processStartDeclarations maps the optional supervision declarations onto the
// flags `workass run` already has. Nothing new is invented: a job started this
// way is the same job a human starts in a shell there.
func processStartDeclarations(params map[string]any) []string {
	out := []string{}
	if role := strings.TrimSpace(fieldString(params, "role")); role != "" {
		out = append(out, "--role", role)
	}
	if resource := strings.TrimSpace(fieldString(params, "resource")); resource != "" {
		out = append(out, "--resource", resource)
	}
	if floor := strings.TrimSpace(fieldString(params, "min_free_disk")); floor != "" {
		out = append(out, "--min-free-disk", floor)
	}
	if inputs := stringSliceField(params["inputs"]); len(inputs) > 0 {
		out = append(out, "--inputs", strings.Join(inputs, ","))
	}
	for _, pair := range []struct {
		field string
		flag  string
	}{
		{"max_runtime_ms", "--max-runtime"},
		{"stale_after_ms", "--stale-after"},
		{"max_hold_ms", "--max-hold"},
	} {
		if value := intField(params, pair.field); value > 0 {
			out = append(out, pair.flag, (time.Duration(value) * time.Millisecond).String())
		}
	}
	return out
}

// processStartEnvironment builds the job's environment from nothing.
//
// This is the whole of §9's promise, kept literally: the daemon's own
// environment is not inherited, not filtered, not partially copied. PATH and
// HOME are read from this process because a program with neither cannot run at
// all, and everything else is what the caller stated by name.
func processStartEnvironment(params map[string]any) ([]string, error) {
	environment := []string{}
	for _, name := range []string{"PATH", "HOME"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	if root := strings.TrimSpace(os.Getenv(procchild.DataRootEnvVar)); root != "" {
		environment = append(environment, procchild.DataRootEnvVar+"="+root)
	}
	// Named one by one, and none is a secret: what the platform needs for the
	// supervisor to reach its service manager, or for a process to start at all.
	for _, name := range procchild.SupervisorEnvironmentNames() {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	declared := mapFromAnyMain(params["env"])
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if !processStartEnvName(trimmed) {
			return nil, fmt.Errorf("env name %q is not a usable environment variable name", name)
		}
		value := fieldString(declared, name)
		lower := strings.ToLower(trimmed + "=" + value)
		for _, marker := range processStartSecretMarkers {
			if strings.Contains(lower, marker) {
				return nil, fmt.Errorf("env %q looks like a secret; a job reads its secrets from a file on the machine that runs it, never from a forwarded command", name)
			}
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("env %q contains a NUL byte", name)
		}
		switch trimmed {
		case procchild.TokenEnvVar, procchild.ContextEnvVar:
			return nil, fmt.Errorf("env %q is the job's own credential and is set by this daemon, not by the caller", name)
		}
		environment = append(environment, trimmed+"="+value)
	}
	return environment, nil
}

func processStartEnvName(name string) bool {
	for index := 0; index < len(name); index++ {
		char := name[index]
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char == '_':
		case char >= '0' && char <= '9' && index > 0:
		default:
			return false
		}
	}
	return true
}

// sortStrings keeps the built environment deterministic, which is what makes a
// start reproducible and a test able to assert it.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// fenceRunCredential keeps a one-shot supervised-job credential to the single
// call it was minted for.
//
// It is an allow-list of exactly one method, checked before anything else a
// request could reach, so a credential that leaks out of a job's own file
// cannot send a message, read a chat, spawn an agent or start another job. The
// binding is checked too: a token minted for one job and its owner reports that
// job for that owner, under no chat, and refuses anything else.
func (h *agentControlHandler) fenceRunCredential(method, ownerKey, tabID, chatID string, params map[string]any) error {
	if h == nil || h.manager == nil {
		return nil
	}
	binding, isRun := h.manager.RunTokenFor(ownerKey)
	if !isRun {
		return nil
	}
	if method != "process.register" {
		return errors.New("this is a one-shot supervised-job credential: it authorizes only workass_register_process_child, for the one job it was minted for")
	}
	if binding.OperationID != strings.TrimSpace(fieldString(params, "operation_id")) {
		return errors.New("this supervised-job credential was minted for a different job")
	}
	if !binding.JobScoped() {
		// Minted when a chat here held the job's row; that job keeps reporting
		// the way it always has until it ends (internal/acp/run_tokens.go).
		if binding.ChatID != strings.TrimSpace(chatID) || binding.TabID != strings.TrimSpace(tabID) {
			return errors.New("this supervised-job credential belongs to a different chat on this machine")
		}
		return nil
	}
	if strings.TrimSpace(chatID) != "" || strings.TrimSpace(tabID) != "" ||
		strings.TrimSpace(fieldString(params, "chat_id")) != "" || strings.TrimSpace(fieldString(params, "tab_id")) != "" {
		return errors.New("this supervised-job credential names no chat, so it cannot act for one")
	}
	if binding.WorkID != strings.TrimSpace(fieldString(params, "work_id")) {
		return errors.New("this supervised-job credential was minted for a different job")
	}
	if binding.Owner != strings.TrimSpace(fieldString(params, "owner")) {
		return errors.New("this supervised-job credential was minted for a different owner")
	}
	return nil
}

// processStartOldHostRefusals are what a daemon too old for this start says.
// Each is fixed text in a build that has already shipped.
var processStartOldHostRefusals = []string{
	"process.start needs host_tab_id + host_chat_id",
	"unknown machine-scoped agent control method",
}

// processStartHostRefusal names a host that cannot take this start.
//
// A host that still requires a chat of its own answers with guidance about two
// fields this tool no longer has, which would send a caller looking for a chat
// to create. Both of those refusals happen before that daemon launches anything.
func processStartHostRefusal(machineID string, err error) error {
	if err == nil {
		return nil
	}
	for _, refusal := range processStartOldHostRefusals {
		if strings.Contains(err.Error(), refusal) {
			return fmt.Errorf("machine %s runs an older Workass that cannot start this job, and nothing was started: it can only hold a job inside a chat of its own, and this version does not ask for one. Update %s, then start the job again", machineID, machineID)
		}
	}
	return err
}
