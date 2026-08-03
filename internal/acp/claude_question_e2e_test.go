package acp

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A subagent's question belongs to the agent that spawned it, not to the user's
// screen: it must be handed back with its choices intact so the parent can answer
// it or ask the user itself, and it must never park a background lane on a human.
func TestSubagentQuestionGoesBackToTheParentInsteadOfTheUser(t *testing.T) {
	root := repoRoot(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not available for Claude native-host test: %v", err)
	}
	cliDir := t.TempDir()
	claude := filepath.Join(cliDir, executableName("claude"))
	writeExecutable(t, claude, nativeNoopScript())
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_CLAUDE_HOST", filepath.Join(root, "scripts", "claude-native-host.mjs"))
	t.Setenv("WORKASS_CLAUDE_SDK_MODULE", filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"))
	t.Setenv("WORKASS_CLAUDE_SESSION_ID", "fixture-subagent-question-session")

	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code", Command: claude, Enabled: true, Badge: "native", CWD: root,
		}},
		DefaultProviderID:  "claude",
		ProviderConfigFile: filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:          events.Broadcast,
		InitTimeout:        5 * time.Second,
		PermissionTimeout:  10 * time.Second,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	session, err := manager.NewSession(context.Background(), SessionOptions{
		TabID: "sub-question-tab", ChatID: "sub-question-chat", ProviderID: "claude", CWD: root,
	})
	if err != nil {
		t.Fatalf("new subagent question session: %v", err)
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "sub-question-tab", ChatID: "sub-question-chat",
		ProviderID: "claude", Prompt: "exercise subagent question",
	})
	if err != nil {
		t.Fatalf("start subagent question turn: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 20*time.Second)
	endJob := jobFromEnd(end)
	if endJob["status"] != "done" {
		t.Fatalf("subagent question turn status = %#v", endJob)
	}

	for _, event := range events.snapshot() {
		if event.channel == "chat:permission-request" {
			t.Fatalf("a subagent's question was put on the user's screen: %#v", event.payload)
		}
	}

	result := fmt.Sprint(endJob["result"])
	// The parent needs the question AND its choices to act on it.
	for _, want := range []string{"¿Pusheo los commits pendientes?", "Sí, pusheá", "No, dejalos locales", "agente principal"} {
		if !strings.Contains(result, want) {
			t.Fatalf("handback lost %q: %q", want, result)
		}
	}
}

// End to end over the real host: the SDK calls canUseTool("AskUserQuestion"),
// the host must ask the CLIENT the model's own question with its own answers as
// the options, and hand the chosen answer back to the model. Before this, the
// client got "run AskUserQuestion?" with allow/reject and no answer could ever
// be given (user report 2026-07-25, chat deploy-fix).
func TestClaudeNativeQuestionReachesTheClientAndItsAnswerReachesTheModel(t *testing.T) {
	root := repoRoot(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not available for Claude native-host test: %v", err)
	}
	cliDir := t.TempDir()
	claude := filepath.Join(cliDir, executableName("claude"))
	writeExecutable(t, claude, nativeNoopScript())
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_CLAUDE_HOST", filepath.Join(root, "scripts", "claude-native-host.mjs"))
	t.Setenv("WORKASS_CLAUDE_SDK_MODULE", filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"))
	t.Setenv("WORKASS_CLAUDE_SESSION_ID", "fixture-question-session")

	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code", Command: claude, Enabled: true, Badge: "native", CWD: root,
		}},
		DefaultProviderID:  "claude",
		ProviderConfigFile: filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:          events.Broadcast,
		InitTimeout:        5 * time.Second,
		PermissionTimeout:  10 * time.Second,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	session, err := manager.NewSession(context.Background(), SessionOptions{
		TabID: "claude-question-tab", ChatID: "claude-question-chat", ProviderID: "claude", CWD: root,
	})
	if err != nil {
		t.Fatalf("new Claude question session: %v", err)
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "claude-question-tab", ChatID: "claude-question-chat",
		ProviderID: "claude", Prompt: "exercise question",
	})
	if err != nil {
		t.Fatalf("start Claude question turn: %v", err)
	}

	request := events.waitFor(t, 20*time.Second, func(event collectedEvent) bool {
		return event.channel == "chat:permission-request"
	}).payload.(map[string]any)

	question := mapFromAny(request["question"])
	if question == nil {
		t.Fatalf("client never received the question: %#v", request)
	}
	if got := asString(question["question"]); got != "¿A qué máquina deployamos primero?" {
		t.Fatalf("question text = %q", got)
	}
	if got := asString(question["header"]); got != "Deploy target" {
		t.Fatalf("header = %q", got)
	}
	// The card's title is the question's header, not the tool name.
	if got := asString(request["title"]); got != "Deploy target" {
		t.Fatalf("card title = %q, want the header instead of the tool name", got)
	}

	options := anySlice(request["options"])
	if len(options) != 3 {
		t.Fatalf("options = %d, want 2 answers + the escape hatch", len(options))
	}
	first := mapFromAny(options[0])
	if asString(first["kind"]) != "answer" || asString(first["name"]) != "El nodo de build" {
		t.Fatalf("first option = %+v", first)
	}
	last := mapFromAny(options[len(options)-1])
	if asString(last["kind"]) != "reject_once" {
		t.Fatalf("escape hatch must stay a reject kind so the timeout fallback lands on it: %+v", last)
	}

	if !manager.PermissionDecide(asString(request["id"]), asString(first["optionId"])) {
		t.Fatal("answering the question was refused")
	}

	end := events.waitJobEnd(t, jobID(job), 20*time.Second)
	endJob := jobFromEnd(end)
	if endJob["status"] != "done" {
		t.Fatalf("question turn status = %#v", endJob)
	}
	result := fmt.Sprint(endJob["result"])
	// PermissionResult offers no way to return a tool result, so the answer
	// rides back as the deny message — what matters is the model reads the choice.
	if !strings.Contains(result, "El nodo de build") {
		t.Fatalf("the answer never reached the model: %q", result)
	}
	if !strings.Contains(result, "deny") {
		t.Fatalf("answer was not delivered through the deny channel: %q", result)
	}
}

// The answer rides back as a denial, so the SDK closes the tool call with
// is_error and the row read "falló" on the very click that worked — the user saw
// "Ask user question · falló" above the answer they had just given (2026-07-26).
// A question the user answered is a completed tool call; one nobody answered is
// not, and that half must keep failing or the fix is just whitewash.
func TestClaudeAnsweredQuestionRowCompletesAndAnUnansweredOneStillFails(t *testing.T) {
	if got := questionRowStatus(t, "answered", 0); got != "completed" {
		t.Fatalf("row status after the user answered = %q, want completed", got)
	}
	if got := questionRowStatus(t, "unanswered", -1); got != "failed" {
		t.Fatalf("row status with no answer given = %q, want failed", got)
	}
}

// Runs one question turn end to end over the real host and returns the terminal
// status of its tool row. `choice` indexes the offered options; -1 picks the last
// one, which is the "answer in the chat" escape hatch.
func questionRowStatus(t *testing.T, name string, choice int) string {
	t.Helper()
	root := repoRoot(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not available for Claude native-host test: %v", err)
	}
	cliDir := t.TempDir()
	claude := filepath.Join(cliDir, executableName("claude"))
	writeExecutable(t, claude, nativeNoopScript())
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_CLAUDE_HOST", filepath.Join(root, "scripts", "claude-native-host.mjs"))
	t.Setenv("WORKASS_CLAUDE_SDK_MODULE", filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"))
	t.Setenv("WORKASS_CLAUDE_SESSION_ID", "fixture-question-row-"+name)

	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code", Command: claude, Enabled: true, Badge: "native", CWD: root,
		}},
		DefaultProviderID:  "claude",
		ProviderConfigFile: filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:          events.Broadcast,
		InitTimeout:        5 * time.Second,
		PermissionTimeout:  10 * time.Second,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	session, err := manager.NewSession(context.Background(), SessionOptions{
		TabID: "question-row-tab-" + name, ChatID: "question-row-chat-" + name, ProviderID: "claude", CWD: root,
	})
	if err != nil {
		t.Fatalf("new question-row session: %v", err)
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "question-row-tab-" + name,
		ChatID: "question-row-chat-" + name, ProviderID: "claude", Prompt: "exercise question",
	})
	if err != nil {
		t.Fatalf("start question-row turn: %v", err)
	}

	request := events.waitFor(t, 20*time.Second, func(event collectedEvent) bool {
		return event.channel == "chat:permission-request"
	}).payload.(map[string]any)
	options := anySlice(request["options"])
	if len(options) == 0 {
		t.Fatalf("question carried no options: %#v", request)
	}
	if choice < 0 {
		choice = len(options) - 1
	}
	if !manager.PermissionDecide(asString(request["id"]), asString(mapFromAny(options[choice])["optionId"])) {
		t.Fatal("deciding the question was refused")
	}
	events.waitJobEnd(t, jobID(job), 20*time.Second)

	status := ""
	for _, event := range events.snapshot() {
		if event.channel != "job:event" {
			continue
		}
		payload := mapFromAny(event.payload)
		update := mapFromAny(payload["event"])
		if asString(update["kind"]) == "tool" && asString(update["id"]) == "tool-question-1" {
			status = asString(update["status"])
		}
	}
	if status == "" {
		t.Fatal("the question never produced a tool row")
	}
	return status
}
