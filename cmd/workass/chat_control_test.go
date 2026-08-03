package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workass/internal/acp"
)

func TestAgentChatStoreUsesExactIdentityAndAtomicDurableQueue(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Agent target", stateDir, "codex", "gpt-5.6-sol[xhigh]", "agent-full-access", true)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	if tabID == "" || chatID == "" || tabID == chatID {
		t.Fatalf("created identity = %#v", created)
	}
	if err := store.AgentConfigureChat(tabID, chatID, stateDir, "codex", "gpt-5.6-sol[xhigh]", "gpt-5.6-sol", "xhigh", "agent-full-access"); err != nil {
		t.Fatal(err)
	}
	if err := store.AgentRenameChat(tabID, "wrong-chat", "must fail"); err == nil {
		t.Fatal("rename accepted a mismatched tabId/chatId pair")
	}
	if err := store.AgentRenameChat(tabID, chatID, "Renamed exact target"); err != nil {
		t.Fatal(err)
	}

	receipt, err := store.AgentEnqueueChat(tabID, chatID, "queued by agent", "auto")
	if err != nil {
		t.Fatal(err)
	}
	head, agentFirst, hasQueue := store.AgentQueueHead(tabID, chatID)
	if !agentFirst || !hasQueue || fieldString(head, "id") != fieldString(receipt, "queueId") {
		t.Fatalf("queue head = %#v agentFirst=%v hasQueue=%v", head, agentFirst, hasQueue)
	}
	opts, err := store.AgentPrepareQueuedTurn(tabID, chatID, fieldString(head, "id"), "native-session")
	if err != nil {
		t.Fatal(err)
	}
	if fieldString(opts, "tabId") != tabID || fieldString(opts, "chatId") != chatID || fieldString(opts, "prompt") != "queued by agent" ||
		fieldString(opts, "userMessageId") == "" || fieldString(opts, "assistantMessageId") == "" ||
		fieldString(opts, "queueId") != fieldString(head, "id") || parseJobStartOptions(opts).QueueID != fieldString(head, "id") {
		t.Fatalf("prepared receipt = %#v", opts)
	}
	read, err := store.AgentReadChat(tabID, chatID, 20, false)
	if err != nil {
		t.Fatal(err)
	}
	messages := anySlice(read["messages"])
	if len(messages) != 2 || fieldString(mapFromAnyMain(messages[0]), "role") != "user" || fieldString(mapFromAnyMain(messages[1]), "status") != "running" {
		t.Fatalf("atomic prepared transcript = %#v", messages)
	}
	if _, _, queued := store.AgentQueueHead(tabID, chatID); queued {
		t.Fatal("prepared queue item was not consumed atomically")
	}

	listed := store.AgentChatList()
	if len(listed) != 1 || fieldString(listed[0], "title") != "Renamed exact target" || fieldString(listed[0], "modelId") != "gpt-5.6-sol[xhigh]" {
		t.Fatalf("chat list = %#v", listed)
	}
	root := store.Get().(map[string]any)
	chat := chatFromSnapshot(root, tabID)
	memory := mapFromAnyMain(mapFromAnyMain(mapFromAnyMain(chat["modelControls"])["codex"])["gpt-5.6-sol"])
	if fieldString(memory, "effort") != "xhigh" || fieldString(memory, "modeId") != "agent-full-access" {
		t.Fatalf("per-model controls = %#v", memory)
	}
}

func TestAgentQueueRevisionRejectsStaleRendererResurrectionAndAllowsCurrentClear(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Queue revision", stateDir, "codex", "gpt-5.6-sol[max]", "agent-full-access", false)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")

	first, err := store.AgentEnqueueChat(tabID, chatID, "deliver once", "auto")
	if err != nil {
		t.Fatal(err)
	}
	staleAfterEnqueue := cloneJSON(store.Get()).(map[string]any)
	staleAfterEnqueue["_workassSave"] = "lean-payload-v2"
	staleChat := chatFromSnapshot(staleAfterEnqueue, tabID)
	if revision := intValue(staleChat["agentQueueRevision"]); revision != 1 {
		t.Fatalf("revision after enqueue = %d, want 1", revision)
	}
	if _, err := store.AgentPrepareQueuedTurn(tabID, chatID, fieldString(first, "queueId"), "native-session"); err != nil {
		t.Fatal(err)
	}
	if !store.Save(staleAfterEnqueue) {
		t.Fatal("stale renderer save after queue consumption was rejected")
	}
	if _, _, queued := store.AgentQueueHead(tabID, chatID); queued {
		t.Fatal("stale renderer save resurrected a consumed agent queue item")
	}
	consumedChat := chatFromSnapshot(store.Get().(map[string]any), tabID)
	if revision := intValue(consumedChat["agentQueueRevision"]); revision != 2 {
		t.Fatalf("revision after consume + stale save = %d, want 2", revision)
	}
	reloadedAfterConsume := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	if !reloadedAfterConsume.Save(staleAfterEnqueue) {
		t.Fatal("post-restart stale renderer save after consume was rejected")
	}
	if _, _, queued := reloadedAfterConsume.AgentQueueHead(tabID, chatID); queued {
		t.Fatal("post-restart stale renderer save resurrected a consumed agent queue item")
	}
	store = reloadedAfterConsume

	second, err := store.AgentEnqueueChat(tabID, chatID, "user may clear this", "auto")
	if err != nil {
		t.Fatal(err)
	}
	current := cloneJSON(store.Get()).(map[string]any)
	current["_workassSave"] = "lean-payload-v2"
	currentChat := chatFromSnapshot(current, tabID)
	if revision := intValue(currentChat["agentQueueRevision"]); revision != 3 {
		t.Fatalf("revision before current clear = %d, want 3", revision)
	}
	staleBeforeClear := cloneJSON(current).(map[string]any)
	currentChat["queue"] = []any{}
	if !store.Save(current) {
		t.Fatal("current renderer clear was rejected")
	}
	if _, _, queued := store.AgentQueueHead(tabID, chatID); queued {
		t.Fatalf("current renderer could not clear agent queue item %s", fieldString(second, "queueId"))
	}
	clearedChat := chatFromSnapshot(store.Get().(map[string]any), tabID)
	if revision := intValue(clearedChat["agentQueueRevision"]); revision != 4 {
		t.Fatalf("revision after current clear = %d, want 4", revision)
	}
	reloadedAfterClear := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	if !reloadedAfterClear.Save(staleBeforeClear) {
		t.Fatal("post-restart stale renderer save after clear was rejected")
	}
	if _, _, queued := reloadedAfterClear.AgentQueueHead(tabID, chatID); queued {
		t.Fatal("post-restart stale renderer save resurrected a user-cleared agent queue item")
	}
}

func TestSessionStoreOldShapeSaveFieldMergesDaemonQueueAndControls(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Old renderer", stateDir, "mock", "mock-deterministic", "ask", false)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	oldShape := cloneJSON(store.Get()).(map[string]any)
	oldChat := chatFromSnapshot(oldShape, tabID)
	delete(oldChat, agentQueueRevisionField)
	delete(oldChat, runtimeControlRevisionField)

	if _, err := store.AgentEnqueueChat(tabID, chatID, "daemon-owned queue", "queue"); err != nil {
		t.Fatal(err)
	}
	if !store.UpdateChatControls(tabID, chatID, "codex", "gpt-5.6-sol[xhigh]", "agent-full-access") {
		t.Fatal("daemon control update")
	}
	oldChat["queue"] = []any{}
	oldChat["providerId"] = "stale-provider"
	oldChat["currentModelId"] = "stale-model"
	oldChat["currentModeId"] = "stale-mode"
	if !store.Save(oldShape) {
		t.Fatal("old-shape session:save was rejected")
	}
	got := chatFromSnapshot(store.Get().(map[string]any), tabID)
	if len(anySlice(got["queue"])) != 1 || fieldString(mapFromAnyMain(anySlice(got["queue"])[0]), "text") != "daemon-owned queue" {
		t.Fatalf("old-shape save replaced daemon queue = %#v", got["queue"])
	}
	if fieldString(got, "providerId") != "codex" ||
		fieldString(got, "currentModelId") != "gpt-5.6-sol[xhigh]" ||
		fieldString(got, "currentModeId") != "agent-full-access" {
		t.Fatalf("old-shape save replaced daemon controls = %#v", got)
	}
}

func TestAgentQueueRevisionPreservesNewServerItemAcrossStaleRendererSave(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Queue preservation", stateDir, "codex", "gpt-5.6-sol[max]", "agent-full-access", false)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	staleBeforeEnqueue := cloneJSON(store.Get()).(map[string]any)
	staleBeforeEnqueue["_workassSave"] = "lean-payload-v2"
	chatFromSnapshot(staleBeforeEnqueue, tabID)["queue"] = []any{
		map[string]any{"id": "renderer-local", "text": "keep this local row"},
	}

	receipt, err := store.AgentEnqueueChat(tabID, chatID, "server-owned", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !store.Save(staleBeforeEnqueue) {
		t.Fatal("stale renderer save before enqueue was rejected")
	}
	head, agentFirst, hasQueue := store.AgentQueueHead(tabID, chatID)
	if !hasQueue || !agentFirst || fieldString(head, "id") != fieldString(receipt, "queueId") {
		t.Fatalf("stale renderer save removed new server queue item: head=%#v agentFirst=%v hasQueue=%v", head, agentFirst, hasQueue)
	}
	queue := anySlice(chatFromSnapshot(store.Get().(map[string]any), tabID)["queue"])
	if len(queue) != 2 || fieldString(mapFromAnyMain(queue[1]), "id") != "renderer-local" {
		t.Fatalf("stale reconciliation lost the concurrent renderer row: %#v", queue)
	}
}

func TestHostBusyQueueRejectsStaleOptimisticResurrectionAndReusesTurnIDs(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	created, err := store.AgentCreateChat("Busy queue", stateDir, "mock", "mock-deterministic", "ask", false)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")

	optimistic := cloneJSON(store.Get()).(map[string]any)
	optimistic["_workassSave"] = "lean-payload-v2"
	chat := chatFromSnapshot(optimistic, tabID)
	chat["messages"] = []any{
		map[string]any{"id": "host-clear-user", "role": "user", "content": "clear this", "status": "done"},
		map[string]any{"id": "host-clear-assistant", "role": "assistant", "content": "", "status": "running"},
	}
	if !store.Save(optimistic) {
		t.Fatal("save first optimistic pair")
	}
	staleBeforeClear := cloneJSON(optimistic).(map[string]any)
	first, err := store.QueueRendererStartCollision(map[string]any{
		"tabId": tabID, "chatId": chatID, "prompt": "clear this",
		"userMessageId": "host-clear-user", "assistantMessageId": "host-clear-assistant",
	})
	if err != nil {
		t.Fatal(err)
	}
	current := cloneJSON(store.Get()).(map[string]any)
	current["_workassSave"] = "lean-payload-v2"
	currentChat := chatFromSnapshot(current, tabID)
	if intValue(currentChat[agentQueueRevisionField]) != 1 || len(anySlice(currentChat["queue"])) != 1 {
		t.Fatalf("first host queue state = %#v", currentChat)
	}
	currentChat["queue"] = []any{}
	if !store.Save(current) {
		t.Fatal("current renderer clear rejected")
	}
	if !store.Save(staleBeforeClear) {
		t.Fatal("stale pre-conversion save after clear rejected")
	}
	afterClear := chatFromSnapshot(store.Get().(map[string]any), tabID)
	if len(anySlice(afterClear["queue"])) != 0 || len(messageSlice(afterClear)) != 0 || intValue(afterClear[agentQueueRevisionField]) != 2 {
		t.Fatalf("stale save resurrected cleared host collision %s: %#v", fieldString(first, "queueId"), afterClear)
	}

	secondOptimistic := cloneJSON(store.Get()).(map[string]any)
	secondOptimistic["_workassSave"] = "lean-payload-v2"
	secondChat := chatFromSnapshot(secondOptimistic, tabID)
	secondChat["messages"] = []any{
		map[string]any{"id": "host-drain-user", "role": "user", "content": "drain once", "status": "done"},
		map[string]any{"id": "host-drain-assistant", "role": "assistant", "content": "", "status": "running"},
	}
	if !store.Save(secondOptimistic) {
		t.Fatal("save second optimistic pair")
	}
	staleBeforeDrain := cloneJSON(secondOptimistic).(map[string]any)
	second, err := store.QueueRendererStartCollision(map[string]any{
		"tabId": tabID, "chatId": chatID, "prompt": "drain once",
		"userMessageId": "host-drain-user", "assistantMessageId": "host-drain-assistant",
		"images": []any{map[string]any{"mimeType": "image/png", "data": "aGVsbG8=", "name": "proof.png"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !store.Save(staleBeforeDrain) {
		t.Fatal("stale save removed queued host collision")
	}
	reloaded := newSessionStore(path)
	head, daemonFirst, hasQueue := reloaded.AgentQueueHead(tabID, chatID)
	if !hasQueue || !daemonFirst || fieldString(head, "id") != fieldString(second, "queueId") || fieldString(head, "source") != "host" {
		t.Fatalf("reloaded host queue = %#v daemonFirst=%v hasQueue=%v", head, daemonFirst, hasQueue)
	}
	opts, err := reloaded.AgentPrepareQueuedTurn(tabID, chatID, fieldString(second, "queueId"), "native-session")
	if err != nil {
		t.Fatal(err)
	}
	if fieldString(opts, "userMessageId") != "host-drain-user" || fieldString(opts, "assistantMessageId") != "host-drain-assistant" || len(anySlice(opts["images"])) != 1 {
		t.Fatalf("host queue turn identity = %#v", opts)
	}
	prepared := chatFromSnapshot(reloaded.Get().(map[string]any), tabID)
	preparedMessages := messageSlice(prepared)
	if len(preparedMessages) != 2 || len(anySlice(mapFromAnyMain(preparedMessages[0])["images"])) != 1 {
		t.Fatalf("prepared host images/messages = %#v", preparedMessages)
	}
	if !reloaded.Save(staleBeforeDrain) {
		t.Fatal("stale save after host consume rejected")
	}
	afterConsume := chatFromSnapshot(reloaded.Get().(map[string]any), tabID)
	if len(anySlice(afterConsume["queue"])) != 0 || len(messageSlice(afterConsume)) != 2 ||
		fieldString(mapFromAnyMain(messageSlice(afterConsume)[1]), "status") != "running" {
		t.Fatalf("stale save corrupted consumed host turn: %#v", afterConsume)
	}
}

func TestAgentLiveSteerMovesDurableQueueIntoCanonicalHistory(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Steer target", stateDir, "codex", "gpt-5.6-sol[xhigh]", "agent-full-access", false)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	receipt, err := store.AgentEnqueueChat(tabID, chatID, "change direction", "steer")
	if err != nil {
		t.Fatal(err)
	}
	staleAfterEnqueue := cloneJSON(store.Get()).(map[string]any)
	staleAfterEnqueue["_workassSave"] = "lean-payload-v2"
	if err := store.AgentCommitLiveSteer(tabID, chatID, fieldString(receipt, "queueId")); err != nil {
		t.Fatal(err)
	}
	if !store.Save(staleAfterEnqueue) {
		t.Fatal("stale renderer save after live steer was rejected")
	}
	read, err := store.AgentReadChat(tabID, chatID, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	messages := anySlice(read["messages"])
	if len(messages) != 1 || fieldString(mapFromAnyMain(messages[0]), "content") != "change direction" {
		t.Fatalf("steer history = %#v", messages)
	}
	if _, _, queued := store.AgentQueueHead(tabID, chatID); queued {
		t.Fatal("stale renderer save resurrected acknowledged live steer")
	}
	archive := loadChatArchive(stateDir, tabID)
	if len(archive) != 1 || fieldString(mapFromAnyMain(archive[0]), "content") != "change direction" {
		t.Fatalf("steer archive = %#v", archive)
	}
}

func TestT4ModelControlKeysMigrateToBaseAndCompositeCreateValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	snapshot := sessionMirrorFixture("polluted-tab", "polluted-chat", "polluted controls")
	chat := chatFromSnapshot(snapshot, "polluted-tab")
	chat["modelControls"] = map[string]any{"mock": map[string]any{
		"mock-deterministic":       map[string]any{"effort": "low", "modeId": "ask"},
		"mock-deterministic[high]": map[string]any{"effort": "high", "modeId": "bypass"},
		"literal[1m]":              map[string]any{"modeId": "literal"},
	}}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal polluted snapshot: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write polluted snapshot: %v", err)
	}
	var logLines []string
	prevLog := sessionStoreLogLine
	sessionStoreLogLine = func(line string) { logLines = append(logLines, line) }
	t.Cleanup(func() { sessionStoreLogLine = prevLog })

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload polluted controls: %v", err)
	}
	gotChat := chatFromSnapshot(reloaded.Get().(map[string]any), "polluted-tab")
	memory := mapFromAnyMain(mapFromAnyMain(gotChat["modelControls"])["mock"])
	if _, exists := memory["mock-deterministic[high]"]; exists {
		t.Fatalf("composite modelControls key survived migration: %#v", memory)
	}
	base := mapFromAnyMain(memory["mock-deterministic"])
	if fieldString(base, "effort") != "low" || fieldString(base, "modeId") != "ask" {
		t.Fatalf("base controls did not win conflict: %#v", base)
	}
	if _, exists := memory["literal[1m]"]; !exists {
		t.Fatalf("literal bracketed adapter id was migrated away: %#v", memory)
	}
	if len(logLines) != 1 || !strings.Contains(logLines[0], "from=mock-deterministic[high]") || !strings.Contains(logLines[0], "to=mock-deterministic") {
		t.Fatalf("migration logs = %#v", logLines)
	}

	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	parent, err := store.AgentCreateChat("Parent", stateDir, "mock", "mock-deterministic[low]", "ask", true)
	if err != nil {
		t.Fatalf("create parent chat: %v", err)
	}
	root := repoRoot(t)
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock",
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	coordinator := newChatControlCoordinator(manager, store, func(string, any) {})
	created, err := coordinator.create(context.Background(), fieldString(parent, "tabId"), fieldString(parent, "chatId"), map[string]any{
		"title": "Composite child", "cwd": stateDir, "provider_id": "mock", "model_id": "mock-deterministic[high]",
	})
	if err != nil {
		t.Fatalf("create chat with composite model: %v", err)
	}
	if created["modelId"] != "mock-deterministic" || created["effort"] != "high" || created["resolvedModelId"] != "mock-deterministic[high]" {
		t.Fatalf("created composite controls = %#v", created)
	}
}

func TestT8EnqueueServerNoticeDrainsAutoQueueAndResumesHibernatedEngine(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Wake target", root, "mock", "mock-deterministic", "ask", true)
	if err != nil {
		t.Fatalf("create wake target: %v", err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	jobEnds := make(chan map[string]any, 4)
	broadcast := daemonEventBroadcaster(store, func(channel string, payload any) {
		if channel != "job:event" {
			return
		}
		event := mapFromAnyMain(payload)
		if fieldString(event, "type") != "end" {
			return
		}
		jobEnds <- mapFromAnyMain(event["job"])
	})
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(stateDir, "acp"), RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID:            "mock",
		HibernateTTL:                 10 * time.Millisecond,
		LifecycleCheckInterval:       10 * time.Millisecond,
		RSSSampleInterval:            time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
		Broadcast:                    broadcast,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root})
	if err != nil {
		t.Fatalf("new wake session: %v", err)
	}
	warmup, err := manager.StartJob(ctx, acp.JobStartOptions{Kind: "app-chat", TabID: tabID, ChatID: chatID, SessionID: session.SessionID, Prompt: "warm up before hibernate"})
	if err != nil {
		t.Fatalf("warmup start: %v", err)
	}
	warmupEnd := waitCmdJobEnd(t, jobEnds, fieldString(warmup, "id"), 5*time.Second)
	if fieldString(warmupEnd, "status") != "done" {
		t.Fatalf("warmup end = %#v", warmupEnd)
	}
	waitCmdProcessState(t, manager, chatID, string(acp.StateHibernated), 3*time.Second)
	if _, live := manager.LiveSession(session.SessionID); live {
		t.Fatalf("old session still live after hibernation: %#v", manager.LiveSessions())
	}

	coordinator := newChatControlCoordinator(manager, store, nil)
	notice := spawnedWorkServerNoticeText([]acp.SpawnedWorkItem{{
		TaskID: "xw-notice", Label: "Lane done", Status: "exited", OutputFile: filepath.Join(stateDir, "lane.output"),
	}})
	if err := coordinator.EnqueueServerNotice(tabID, chatID, notice); err != nil {
		t.Fatalf("enqueue server notice: %v", err)
	}
	noticeEnd := waitAnyCmdJobEndExcept(t, jobEnds, fieldString(warmup, "id"), 6*time.Second)
	if fieldString(noticeEnd, "status") != "done" || fieldString(noticeEnd, "chatId") != chatID || fieldString(noticeEnd, "tabId") != tabID {
		t.Fatalf("notice turn end = %#v", noticeEnd)
	}
	if _, _, queued := store.AgentQueueHead(tabID, chatID); queued {
		t.Fatal("server notice remained queued after drain")
	}
	read, err := store.AgentReadChat(tabID, chatID, 20, false)
	if err != nil {
		t.Fatalf("read wake chat: %v", err)
	}
	foundNotice := false
	for _, raw := range anySlice(read["messages"]) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "role") == "user" && strings.Contains(fieldString(message, "content"), "Background work completed while no turn was active") &&
			strings.Contains(fieldString(message, "content"), "Lane done — exited") {
			foundNotice = true
		}
	}
	if !foundNotice {
		t.Fatalf("server notice was not committed into chat history: %#v", read["messages"])
	}
	if len(manager.LiveSessions()) == 0 {
		t.Fatal("server notice did not resume a live session")
	}
}

func TestDaemonQueueStaysDurableWhenAnotherTurnAlreadyOwnsStart(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Busy FIFO", root, "mock", "mock-deterministic", "ask", true)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	jobEnds := make(chan map[string]any, 4)
	broadcast := daemonEventBroadcaster(store, func(channel string, payload any) {
		if channel == "job:event" && fieldString(mapFromAnyMain(payload), "type") == "end" {
			jobEnds <- mapFromAnyMain(mapFromAnyMain(payload)["job"])
		}
	})
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(stateDir, "acp"), RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "750"}, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour, Broadcast: broadcast,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	store.PrepareTurn(map[string]any{
		"tabId": tabID, "chatId": chatID, "prompt": "[mock:slow] active owner",
		"userMessageId": "active-user", "assistantMessageId": "active-assistant",
	})
	active, err := manager.StartJob(ctx, acp.JobStartOptions{
		Kind: "app-chat", TabID: tabID, ChatID: chatID, SessionID: session.SessionID, CWD: root,
		ProviderID: "mock", Prompt: "[mock:slow] active owner", UserMessageID: "active-user", AssistantMessageID: "active-assistant",
	})
	if err != nil {
		t.Fatalf("start active owner: %v", err)
	}
	receipt, err := store.AgentEnqueueChat(tabID, chatID, "deliver after active owner", "queue")
	if err != nil {
		t.Fatal(err)
	}
	head, daemonFirst, hasQueue := store.AgentQueueHead(tabID, chatID)
	if !hasQueue || !daemonFirst {
		t.Fatalf("queued head = %#v daemonFirst=%v hasQueue=%v", head, daemonFirst, hasQueue)
	}
	coordinator := newChatControlCoordinator(manager, store, nil)
	err = coordinator.startQueuedTurn(ctx, tabID, chatID, head)
	if !errors.Is(err, acp.ErrChatBusy) {
		t.Fatalf("busy daemon queue start error = %v, want ErrChatBusy", err)
	}
	stillQueued, daemonFirst, hasQueue := store.AgentQueueHead(tabID, chatID)
	if !hasQueue || !daemonFirst || fieldString(stillQueued, "id") != fieldString(receipt, "queueId") {
		t.Fatalf("busy start consumed FIFO: head=%#v daemonFirst=%v hasQueue=%v", stillQueued, daemonFirst, hasQueue)
	}
	read, err := store.AgentReadChat(tabID, chatID, 20, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range anySlice(read["messages"]) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "status") == "failed" && strings.Contains(fieldString(message, "content"), "respuesta en curso") {
			t.Fatalf("busy FIFO became a false failed turn: %#v", message)
		}
	}
	coordinator.scheduleDrain(tabID, chatID)
	time.Sleep(150 * time.Millisecond)
	coordinator.mu.Lock()
	drainingWhileBusy := coordinator.draining[tabID+"\x00"+chatID]
	coordinator.mu.Unlock()
	if drainingWhileBusy {
		t.Fatal("drainer remained parked while another turn owned the chat")
	}
	if !manager.CancelJob(fieldString(active, "id")) {
		t.Fatal("cancel active owner")
	}
	waitCmdJobEnd(t, jobEnds, fieldString(active, "id"), 5*time.Second)
	queuedEnd := waitAnyCmdJobEndExcept(t, jobEnds, fieldString(active, "id"), 6*time.Second)
	if fieldString(queuedEnd, "status") != "done" {
		t.Fatalf("queued turn end = %#v", queuedEnd)
	}
	if _, _, hasQueue := store.AgentQueueHead(tabID, chatID); hasQueue {
		t.Fatal("queued turn remained after serialized drain")
	}
}

func TestRendererQueueHeadIsObservedAdoptedAndCannotResurrect(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Renderer adoption", stateDir, "mock", "mock-deterministic", "ask", false)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	manager := acp.NewManager(acp.Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	coordinator := newChatControlCoordinator(manager, store, nil)
	coordinator.rendererAdoptionAge = 30 * time.Millisecond
	coordinator.rendererRecheckBase = 5 * time.Millisecond
	coordinator.rendererRecheckMax = 10 * time.Millisecond
	var attempts atomic.Int32
	coordinator.startQueuedTurnOverride = func(_ context.Context, gotTabID, gotChatID string, item map[string]any) error {
		attempts.Add(1)
		_, prepareErr := store.AgentPrepareQueuedTurn(gotTabID, gotChatID, fieldString(item, "id"), "native-session")
		return prepareErr
	}

	rendererSave := cloneJSON(store.Get()).(map[string]any)
	rendererChat := chatFromSnapshot(rendererSave, tabID)
	rendererChat["queue"] = []any{map[string]any{
		"id": "renderer-row", "text": "adopt exactly once", rendererQueueObservedField: "2000-01-01T00:00:00Z",
	}}
	if !store.Save(rendererSave) {
		t.Fatal("save renderer queue row")
	}
	staleRendererSave := cloneJSON(store.Get()).(map[string]any)
	staleRow := mapFromAnyMain(anySlice(chatFromSnapshot(staleRendererSave, tabID)["queue"])[0])
	if fieldString(staleRow, rendererQueueObservedField) == "2000-01-01T00:00:00Z" {
		t.Fatal("renderer was allowed to forge its observedAt age")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if attempts.Load() == 1 {
			if _, _, queued := store.AgentQueueHead(tabID, chatID); !queued {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if attempts.Load() != 1 {
		t.Fatalf("adopted renderer row start attempts = %d, want 1", attempts.Load())
	}
	chat := chatFromSnapshot(store.Get().(map[string]any), tabID)
	ledger := anySlice(chat[adoptedQueueLedgerField])
	if len(ledger) != 1 || stringValue(ledger[0]) != "renderer-row" {
		t.Fatalf("adopted ledger = %#v", ledger)
	}
	messages := messageSlice(chat)
	if len(messages) != 2 || fieldString(mapFromAnyMain(messages[0]), agentQueueMessageField) != "renderer-row" {
		t.Fatalf("adopted canonical turn = %#v", messages)
	}
	if !store.Save(staleRendererSave) {
		t.Fatal("save stale renderer queue after adoption")
	}
	if _, _, queued := store.AgentQueueHead(tabID, chatID); queued {
		t.Fatal("stale renderer save resurrected an adopted queue row")
	}
	if store.PrepareTurn(map[string]any{
		"tabId": tabID, "chatId": chatID, "prompt": "duplicate renderer start",
		"queueId": "renderer-row", "userMessageId": "duplicate-user", "assistantMessageId": "duplicate-assistant",
	}) {
		t.Fatal("PrepareTurn accepted an originating id already in the adopted ledger")
	}
	collision, err := store.QueueRendererStartCollision(map[string]any{
		"tabId": tabID, "chatId": chatID, "prompt": "duplicate renderer start",
		"queueId": "renderer-row", "userMessageId": "duplicate-user", "assistantMessageId": "duplicate-assistant",
	})
	if err != nil || collision["adopted"] != true || fieldString(collision, "queueId") != "renderer-row" {
		t.Fatalf("adopted collision receipt = %#v, err=%v", collision, err)
	}
}

func TestRendererQueueAdoptionSkipsPendingAttachments(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Pending attachment", stateDir, "mock", "mock-deterministic", "ask", false)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	snapshot := cloneJSON(store.Get()).(map[string]any)
	chatFromSnapshot(snapshot, tabID)["queue"] = []any{map[string]any{
		"id": "pending-renderer-row", "text": "must not send text-only", "attachmentState": "preparing",
	}}
	if !store.Save(snapshot) {
		t.Fatal("save pending attachment queue")
	}
	manager := acp.NewManager(acp.Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	coordinator := newChatControlCoordinator(manager, store, nil)
	coordinator.rendererAdoptionAge = 0
	coordinator.rendererRecheckBase = time.Hour
	var attempts atomic.Int32
	coordinator.startQueuedTurnOverride = func(context.Context, string, string, map[string]any) error {
		attempts.Add(1)
		return nil
	}
	coordinator.scheduleDrain(tabID, chatID)
	time.Sleep(50 * time.Millisecond)
	if attempts.Load() != 0 {
		t.Fatalf("pending attachment row started %d times", attempts.Load())
	}
	head, daemonOwned, queued := store.AgentQueueHead(tabID, chatID)
	if !queued || daemonOwned || fieldString(head, "source") != "" {
		t.Fatalf("pending attachment row was adopted: head=%#v daemonOwned=%v queued=%v", head, daemonOwned, queued)
	}
}

func TestAgentQueueStartFailureRetriesThenParksAndEnqueuesNotice(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Park failed queue", stateDir, "mock", "mock-deterministic", "ask", false)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	receipt, err := store.AgentEnqueueChat(tabID, chatID, "preserve failed start", "queue")
	if err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(acp.Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	coordinator := newChatControlCoordinator(manager, store, nil)
	coordinator.queueRetryBase = 5 * time.Millisecond
	var failedAttempts atomic.Int32
	var noticeAttempts atomic.Int32
	coordinator.startQueuedTurnOverride = func(_ context.Context, gotTabID, gotChatID string, item map[string]any) error {
		if item["queueNotice"] == true {
			noticeAttempts.Add(1)
			_, prepareErr := store.AgentPrepareQueuedTurn(gotTabID, gotChatID, fieldString(item, "id"), "native-session")
			return prepareErr
		}
		failedAttempts.Add(1)
		return errors.New("provider start failed")
	}
	coordinator.scheduleDrain(tabID, chatID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		head, _, queued := store.AgentQueueHead(tabID, chatID)
		if queued && fieldString(head, "id") == fieldString(receipt, "queueId") && head[queueParkedField] == true && noticeAttempts.Load() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if failedAttempts.Load() != agentQueueStartAttemptLimit {
		t.Fatalf("failed start attempts = %d, want %d", failedAttempts.Load(), agentQueueStartAttemptLimit)
	}
	head, _, queued := store.AgentQueueHead(tabID, chatID)
	if !queued || fieldString(head, "id") != fieldString(receipt, "queueId") || head[queueParkedField] != true {
		t.Fatalf("failed queue row was not parked: %#v", head)
	}
	read, err := store.AgentReadChat(tabID, chatID, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	foundNotice := false
	for _, raw := range anySlice(read["messages"]) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "role") == "user" && strings.Contains(fieldString(message, "content"), "queued turn was parked") {
			foundNotice = true
		}
	}
	if !foundNotice {
		t.Fatalf("parked queue did not enqueue a server notice: %#v", read["messages"])
	}
}

func TestAgentQueueDrainRetriesPendingWakeAfterTimedOutStart(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Pending drain wake", root, "mock", "mock-deterministic", "ask", true)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	if _, err := store.AgentEnqueueChat(tabID, chatID, "deliver after blocked start", "auto"); err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(stateDir, "acp"), RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	coordinator := newChatControlCoordinator(manager, store, nil)
	coordinator.queueStartTimeout = 40 * time.Millisecond
	var attempts atomic.Int32
	firstStarted := make(chan struct{})
	coordinator.startQueuedTurnOverride = func(ctx context.Context, gotTabID, gotChatID string, item map[string]any) error {
		if attempts.Add(1) == 1 {
			close(firstStarted)
			<-ctx.Done()
			return ctx.Err()
		}
		_, prepareErr := store.AgentPrepareQueuedTurn(gotTabID, gotChatID, fieldString(item, "id"), "native-session")
		return prepareErr
	}
	coordinator.scheduleDrain(tabID, chatID)
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first drain attempt did not start")
	}
	// This signal lands while the first start is blocked. It must not disappear
	// merely because the per-chat drainer was already marked active.
	coordinator.scheduleDrain(tabID, chatID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if attempts.Load() >= 2 {
			if _, _, queued := store.AgentQueueHead(tabID, chatID); !queued {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pending wake was lost: attempts=%d", attempts.Load())
}

func waitCmdJobEnd(t *testing.T, ch <-chan map[string]any, id string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case job := <-ch:
			if fieldString(job, "id") == id {
				return job
			}
		case <-deadline:
			t.Fatalf("timed out waiting for job end %s", id)
		}
	}
}

func waitAnyCmdJobEndExcept(t *testing.T, ch <-chan map[string]any, excludedID string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case job := <-ch:
			if fieldString(job, "id") != excludedID {
				return job
			}
		case <-deadline:
			t.Fatalf("timed out waiting for next job end after %s", excludedID)
		}
	}
}

func waitCmdProcessState(t *testing.T, manager *acp.Manager, chatID, state string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, proc := range manager.Processes() {
			if fieldString(proc, "chatId") == chatID && fieldString(proc, "state") == state {
				return proc
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for process state %s chat=%s processes=%#v", state, chatID, manager.Processes())
	return nil
}
