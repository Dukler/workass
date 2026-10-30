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
//   - it is still `workass run` that supervises, in its own process group, so
//     the child is not a child of the daemon and survives a restart or a
//     self-update exactly as a hand-started one does;
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
	if h == nil || h.manager == nil || h.chats == nil || h.chats.providerChats == nil {
		return nil, errors.New("supervised jobs require the durable chat actor")
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

	// The chat on THIS machine that holds the job's local row. It is not the
	// owner — the owner is the chat that asked, on its own machine — but a row
	// needs a chat here, because the row IS this machine's record of the work.
	hostTabID := strings.TrimSpace(fieldString(params, "host_tab_id"))
	hostChatID := strings.TrimSpace(fieldString(params, "host_chat_id"))
	if hostTabID == "" || hostChatID == "" {
		return nil, errors.New("process.start needs host_tab_id + host_chat_id: the exact chat on this machine that will hold the job's local row. Call workass_list_chats with this machine_id, or create one with workass_create_chat machine_id")
	}
	if err := h.fenceActor(hostTabID, hostChatID); err != nil {
		return nil, err
	}
	if _, err := h.chats.providerChats.ReadChat(hostTabID, hostChatID, 1, false); err != nil {
		return nil, errors.New("host_tab_id + host_chat_id do not name a chat on this machine")
	}

	owner, err := fleetid.New(forwarded.Origin.Machine, fleetid.Chat, forwarded.Origin.ChatID)
	if err != nil {
		return nil, fmt.Errorf("the asking chat is not a resolvable fleet id: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("this daemon cannot locate its own executable, so it cannot start `workass run`: %w", err)
	}
	credentialPath, err := h.writeRunCredential(hostTabID, hostChatID, operationID, childID)
	if err != nil {
		return nil, err
	}

	arguments := processStartArguments(label, owner.String(), operationID, cwd, credentialPath, params, command)
	cmd := exec.Command(executable, arguments...)
	cmd.Dir = cwd
	cmd.Env = environment
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Its own process group: the supervisor is not a child of the daemon's
	// lifetime, which is what makes the job survive a restart or a self-update.
	procchild.DetachProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start `workass run` on this machine: %s", acp.RedactSensitiveText(err.Error()))
	}
	supervisorPID := cmd.Process.Pid
	// Reaped off-thread so this daemon leaves no zombie behind; the job's
	// outcome never comes from here, it comes from the spool the supervisor
	// writes and the registration it makes.
	go func() { _ = cmd.Wait() }()

	return map[string]any{
		"ok": true, "workId": childID, "operationId": operationID,
		"machineId": h.selfMachineID, "supervisorPid": supervisorPID,
		"owner": owner.String(), "hostTabId": hostTabID, "hostChatId": hostChatID,
		"startedAt": time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// writeRunCredential puts one job's one-shot credential where only that job can
// read it. `workass run` unlinks a --token-file it consumed, and the token
// itself is revoked by the registration it authorized, so the file and the
// grant both end with the job's first report.
func (h *agentControlHandler) writeRunCredential(tabID, chatID, operationID, childID string) (string, error) {
	template, err := h.manager.ToolContextTemplate()
	if err != nil {
		return "", err
	}
	token := h.manager.MintRunToken(chatID, tabID, operationID)
	if token == "" {
		return "", errors.New("this daemon could not mint a credential for the job")
	}
	template.Credential = token
	template.ChatID = chatID
	template.TabID = tabID
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
func processStartArguments(label, owner, operationID, cwd, credentialPath string, params map[string]any, command []string) []string {
	arguments := []string{"run", "--quiet", "--label", label, "--owner", owner,
		"--operation-id", operationID, "--cwd", cwd, "--token-file", credentialPath}
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
	if output := strings.TrimSpace(fieldString(params, "output_file")); output != "" {
		out = append(out, "--output-file", output)
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
// binding is checked too: a token minted for one job and one chat on this
// machine registers that job in that chat, and refuses anything else.
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
	if binding.ChatID != strings.TrimSpace(chatID) || binding.TabID != strings.TrimSpace(tabID) {
		return errors.New("this supervised-job credential belongs to a different chat on this machine")
	}
	if binding.OperationID != strings.TrimSpace(fieldString(params, "operation_id")) {
		return errors.New("this supervised-job credential was minted for a different job")
	}
	return nil
}
