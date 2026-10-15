package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// claudeForkStrategy forks a Claude Code session with the official CLI:
// `--resume <session> --fork-session` copies the exact history into a new
// session, so the API prompt cache serves the shared prefix.
type claudeForkStrategy struct{}

func (claudeForkStrategy) Capability() map[string]any {
	return map[string]any{
		"supported": true,
		"mechanism": "claude --resume <seal> --fork-session, after one sealing turn forked from the parent (--resume <parent> --fork-session)",
		"evidence":  "2026-10-15, Claude Code 2.1.300, Sonnet 5.5: children forked from the seal read 28,915 of 28,971 prefix tokens from cache (cache_creation 54); children forked straight from the parent re-cached ~7.9k tokens each",
		"notes":     "Children run with no MCP servers so every request carries an identical tool list. Up to 16 images per throwaway child (8 per kept child, whose persisted session adds a note block per image) keep the fork point inside the provider's ~20-block cache lookback; more still run but re-cache the shared prefix. Throwaway children run without session persistence, so they leave no native session behind. Claude Code itself re-encodes PNG inputs above ~500 KB to JPEG at the same pixel size before sending; Workass never resizes.",
	}
}

func (claudeForkStrategy) NativeStore(extraEnv map[string]string) string {
	if dir := strings.TrimSpace(extraEnv["CLAUDE_CONFIG_DIR"]); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".claude")
	}
	return ""
}

var claudeSessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// DiscardSession deletes a throwaway child's own Claude Code session (a full
// copy of the parent history, images included). Only the exact UUID-named
// transcript and its sidecar directory are touched.
func (claudeForkStrategy) DiscardSession(store, sessionID string) {
	sessionID = strings.ToLower(strings.TrimSpace(sessionID))
	store = strings.TrimSpace(store)
	if store == "" || !claudeSessionIDPattern.MatchString(sessionID) {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(store, "projects", "*", sessionID+".jsonl"))
	for _, match := range matches {
		_ = os.Remove(match)
		sidecar := strings.TrimSuffix(match, ".jsonl")
		if info, err := os.Stat(sidecar); err == nil && info.IsDir() && filepath.Base(sidecar) == sessionID {
			_ = os.RemoveAll(sidecar)
		}
	}
}

// claudeForkArgs pins everything that shapes the request prefix. The seal and
// every child of a fork point must send byte-identical tools and system
// content, so MCP servers are disabled outright: their connection timing
// otherwise changed the tool list between the seal and a child.
func claudeForkArgs(spec forkTurnSpec) []string {
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--resume", spec.ResumeSessionID, "--fork-session",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
	}
	if model := strings.TrimSpace(spec.ModelID); model != "" && !strings.EqualFold(model, "default") {
		args = append(args, "--model", model)
	}
	if effort := strings.TrimSpace(spec.Effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	if mode := strings.TrimSpace(spec.ModeID); mode != "" {
		args = append(args, "--permission-mode", mode)
		if mode == "bypassPermissions" {
			args = append(args, "--allow-dangerously-skip-permissions")
		}
	}
	for _, dir := range spec.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	// A persisted session adds one "[Image: source: …]" note block per image
	// to the user turn. With 12 images that is 25 blocks after the fork point,
	// beyond the API's ~20-block cache lookback, so the whole history was
	// re-cached (measured 2026-10-15: 14,641 tokens read with persistence,
	// 44,786 without, same message).
	if !spec.Persist {
		args = append(args, "--no-session-persistence")
	}
	return args
}

func (claudeForkStrategy) RunTurn(ctx context.Context, spec forkTurnSpec) (forkTurnResult, error) {
	if strings.TrimSpace(spec.ResumeSessionID) == "" {
		return forkTurnResult{}, errors.New("fork turn has no session to resume")
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultForkChildTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	line, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": spec.Content}})
	if err != nil {
		return forkTurnResult{}, err
	}
	cmd := managedCommand(spec.Launch.Command, claudeForkArgs(spec)...)
	cmd.Dir = spec.CWD
	// claude.ai connectors attach asynchronously and would change the tool
	// list between the seal and a child.
	cmd.Env = append(append([]string(nil), spec.Launch.Env...), "ENABLE_CLAUDEAI_MCP_SERVERS=false")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return forkTurnResult{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return forkTurnResult{}, err
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		return forkTurnResult{}, err
	}
	cmd.Stderr = stderrWriter
	tree, err := startProcessTree(cmd)
	_ = stderrWriter.Close()
	if err != nil {
		_ = stderrReader.Close()
		return forkTurnResult{}, fmt.Errorf("start %s: %w", filepath.Base(spec.Launch.Command), err)
	}
	stderrTail := &boundedTail{limit: forkStderrTailBytes}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		defer stderrReader.Close()
		_, _ = io.Copy(stderrTail, stderrReader)
	}()
	go func() {
		_, _ = stdin.Write(append(line, '\n'))
		_ = stdin.Close()
	}()

	recorder := newForkTranscript(spec.TranscriptPath)
	recorder.write(map[string]any{"kind": "user", "text": spec.UserText, "attachments": transcriptAttachments(spec.UserAttachments)})

	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = stopProcessTree(cmd.Process, tree)
		case <-stopped:
		}
	}()

	result := forkTurnResult{}
	sawResult := false
	// One model call arrives as one event per content block, each carrying
	// the usage so far under the same message id; the last one is final.
	callID, callUsage, calls := "", map[string]any(nil), 0
	flushCall := func() {
		if callID == "" {
			return
		}
		calls++
		// Input-side counts are fixed when the call starts; the streamed output
		// count can be partial, so only the turn total (from result) is kept.
		recorder.write(map[string]any{"kind": "call", "call": calls, "usage": map[string]any{
			"input":     int64Field(callUsage, "input_tokens"),
			"cacheRead": int64Field(callUsage, "cache_read_input_tokens"), "cacheCreation": int64Field(callUsage, "cache_creation_input_tokens"),
		}})
		callID = ""
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch asString(event["type"]) {
		case "system":
			if asString(event["subtype"]) == "init" {
				if id := asString(event["session_id"]); id != "" && result.SessionID == "" {
					result.SessionID = id
					if spec.OnSession != nil {
						spec.OnSession(id)
					}
				}
			}
		case "assistant":
			if asString(event["parent_tool_use_id"]) != "" {
				continue
			}
			message := mapFromAny(event["message"])
			if usage := mapFromAny(message["usage"]); len(usage) > 0 {
				result.LastCallContext = int64Field(usage, "input_tokens") + int64Field(usage, "cache_read_input_tokens") + int64Field(usage, "cache_creation_input_tokens")
				if id := asString(message["id"]); id != "" {
					if id != callID {
						flushCall()
						callID = id
					}
					callUsage = usage
				}
			}
			for _, raw := range anyList(message["content"]) {
				block := mapFromAny(raw)
				switch asString(block["type"]) {
				case "text":
					if text := strings.TrimSpace(asString(block["text"])); text != "" {
						recorder.write(map[string]any{"kind": "assistant", "text": RedactSensitiveText(text)})
						if spec.OnActivity != nil {
							spec.OnActivity("Respondiendo")
						}
					}
				case "tool_use":
					name := asString(block["name"])
					input := mapFromAny(block["input"])
					target := forkToolTarget(input)
					recorder.write(map[string]any{"kind": "tool", "id": asString(block["id"]), "name": name, "target": target, "input": boundedToolInput(input)})
					if spec.OnActivity != nil {
						spec.OnActivity(strings.TrimSpace(name + " " + target))
					}
				}
			}
		case "user":
			if asString(event["parent_tool_use_id"]) != "" {
				continue
			}
			flushCall()
			message := mapFromAny(event["message"])
			for _, raw := range anyList(message["content"]) {
				block := mapFromAny(raw)
				if asString(block["type"]) != "tool_result" {
					continue
				}
				isError, _ := block["is_error"].(bool)
				text, isString := block["content"].(string)
				if !isString {
					text = textFromContent(block["content"])
				}
				recorder.write(map[string]any{
					"kind": "tool_result", "toolUseId": asString(block["tool_use_id"]), "isError": isError,
					"text": compactText(RedactSensitiveText(text), forkTranscriptToolText),
				})
			}
		case "result":
			flushCall()
			sawResult = true
			result.Subtype = asString(event["subtype"])
			result.IsError, _ = event["is_error"].(bool)
			result.Result = asString(event["result"])
			result.Turns = int(int64Field(event, "num_turns"))
			if id := asString(event["session_id"]); id != "" {
				result.SessionID = id
			}
			usage := mapFromAny(event["usage"])
			result.Usage = ForkUsage{
				InputTokens: int64Field(usage, "input_tokens"), OutputTokens: int64Field(usage, "output_tokens"),
				CacheReadTokens: int64Field(usage, "cache_read_input_tokens"), CacheCreationTokens: int64Field(usage, "cache_creation_input_tokens"),
			}
			recorder.write(map[string]any{
				"kind": "result", "subtype": result.Subtype, "isError": result.IsError, "turns": result.Turns,
				"durationMs": int64Field(event, "duration_ms"), "text": RedactSensitiveText(result.Result),
				"usage": map[string]any{
					"input": result.Usage.InputTokens, "output": result.Usage.OutputTokens,
					"cacheRead": result.Usage.CacheReadTokens, "cacheCreation": result.Usage.CacheCreationTokens,
				},
			})
		}
	}
	waitErr := cmd.Wait()
	close(stopped)
	_ = releaseProcessTree(tree)
	<-stderrDone
	recorder.close()
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("timed out after %s", timeout.Round(time.Second))
		}
		return result, ctx.Err()
	}
	if !sawResult {
		tail := strings.TrimSpace(RedactSensitiveText(stderrTail.String()))
		if waitErr != nil {
			return result, fmt.Errorf("provider exited without a result (%v): %s", waitErr, compactText(tail, 600))
		}
		return result, fmt.Errorf("provider exited without a result: %s", compactText(tail, 600))
	}
	return result, nil
}
