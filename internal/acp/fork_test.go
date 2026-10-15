package acp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeTestPNG(t *testing.T, path string, width, height int) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidateImageAttachmentPaths(t *testing.T) {
	dir := t.TempDir()
	writeTestPNG(t, filepath.Join(dir, "ok.png"), 852, 720)
	writeTestPNG(t, filepath.Join(dir, "wide.png"), 1200, 40)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	attachments, err := ValidateImageAttachmentPaths([]string{"ok.png"}, dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 || attachments[0].Width != 852 || attachments[0].Height != 720 || attachments[0].MIME != "image/png" || attachments[0].SHA256 == "" {
		t.Fatalf("attachment = %+v", attachments)
	}
	cases := map[string][]string{
		"exceeds the 1000 px limit":  {"wide.png"},
		"file does not exist":        {"missing.png"},
		"not a PNG, JPEG, WebP":      {"notes.txt"},
		"the same file is attached":  {"ok.png", filepath.Join(dir, "ok.png")},
		"relative path needs":        {"ok.png"},
		"too many image attachments": make([]string, MaxImageAttachments+1),
	}
	for want, paths := range cases {
		base := dir
		if want == "relative path needs" {
			base = ""
		}
		if _, err := ValidateImageAttachmentPaths(paths, base, 0); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want %q", paths, err, want)
		}
	}
	if _, err := ValidateImageAttachmentPaths([]string{"wide.png"}, dir, 1300); err != nil {
		t.Fatalf("explicit edge limit: %v", err)
	}
	if _, err := ValidateImageAttachmentPaths([]string{"ok.png"}, dir, 2000); err == nil {
		t.Fatal("edge limit above the provider ceiling was accepted")
	}
	// A file edited after validation is refused at delivery.
	writeTestPNG(t, filepath.Join(dir, "ok.png"), 10, 10)
	if _, err := LoadImageAttachment(attachments[0]); err == nil || !strings.Contains(err.Error(), "changed after it was validated") {
		t.Fatalf("changed file err = %v", err)
	}
}

func TestWebPDimensions(t *testing.T) {
	header := func(chunk string, body []byte) []byte {
		data := append([]byte("RIFF\x00\x00\x00\x00WEBP"+chunk+"\x00\x00\x00\x00"), body...)
		return append(data, make([]byte, 16)...)
	}
	lossless := make([]byte, 5)
	lossless[0] = 0x2f
	binary.LittleEndian.PutUint32(lossless[1:], uint32(851)|uint32(719)<<14)
	if w, h, err := webpDimensions(header("VP8L", lossless)); err != nil || w != 852 || h != 720 {
		t.Fatalf("VP8L = %d x %d, %v", w, h, err)
	}
	extended := []byte{0, 0, 0, 0, 0xb0, 0x04, 0, 0x7f, 0x02, 0}
	if w, h, err := webpDimensions(header("VP8X", extended)); err != nil || w != 1201 || h != 640 {
		t.Fatalf("VP8X = %d x %d, %v", w, h, err)
	}
}

type fakeForkTurns struct {
	mu      sync.Mutex
	specs   []forkTurnSpec
	block   map[string]chan struct{}
	started chan string
	seq     int
}

func (f *fakeForkTurns) run(ctx context.Context, spec forkTurnSpec) (forkTurnResult, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.seq++
	session := "00000000-0000-4000-8000-" + strings.Repeat("0", 11) + string(rune('a'+f.seq%26))
	text := ""
	for _, raw := range spec.Content {
		if block, ok := raw.(map[string]any); ok && block["type"] == "text" {
			text = block["text"].(string)
			break
		}
	}
	gate := f.block[text]
	f.mu.Unlock()
	if f.started != nil {
		f.started <- text
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return forkTurnResult{SessionID: session}, ctx.Err()
		}
	}
	usage := ForkUsage{InputTokens: 4, OutputTokens: 120, CacheReadTokens: 28900, CacheCreationTokens: 9600}
	if strings.Contains(strings.ToLower(text), "fork point «") && strings.Contains(text, "Reply with exactly: OK") {
		usage.CacheCreationTokens = 60
	}
	return forkTurnResult{SessionID: session, Result: "done: " + text, Turns: 2, Usage: usage, LastCallContext: 28971}, nil
}

func newTestForkService(t *testing.T, turns *fakeForkTurns) (*ForkService, string) {
	t.Helper()
	state := t.TempDir()
	service := NewForkService(state, func(providerID string) (ForkLaunch, error) {
		return ForkLaunch{ProviderID: providerID, Command: "claude", NativeStore: filepath.Join(state, "claude-config")}, nil
	}, nil)
	service.runTurn = turns.run
	return service, state
}

func testForkParent(cwd string) ForkParent {
	return ForkParent{
		ChatID: "chat-1", TabID: "tab-1", ProviderID: "claude", ModelID: "sonnet", Effort: "medium",
		ModeID: "bypassPermissions", CWD: cwd, NativeSessionID: "11111111-1111-4111-8111-111111111111", AfterMessageID: "m-7",
	}
}

func TestForkServiceSealsThenForksChildrenFromTheSeal(t *testing.T) {
	turns := &fakeForkTurns{}
	service, state := newTestForkService(t, turns)
	cwd := t.TempDir()
	for _, name := range []string{"r01.png", "r02.png"} {
		writeTestPNG(t, filepath.Join(cwd, name), 852, 662)
	}
	point, err := service.CreateForkPoint(context.Background(), testForkParent(cwd), ForkPointOptions{Name: "brief+ejemplos", OperationID: "op-point"})
	if err != nil {
		t.Fatal(err)
	}
	if point.Status != ForkStatusReady || point.SealSessionID == "" || point.PrefixTokens != 28971 {
		t.Fatalf("point = %+v", point)
	}
	if seal := turns.specs[0]; seal.ResumeSessionID != "11111111-1111-4111-8111-111111111111" || seal.ModelID != "sonnet" || seal.Effort != "medium" || seal.CWD != cwd || !seal.Persist {
		t.Fatalf("seal spec = %+v", seal)
	}
	// The second hop resumes the first, so the last cache write comes from a
	// resumed request exactly like every child.
	if len(point.SealHops) != 2 || turns.specs[1].ResumeSessionID == turns.specs[0].ResumeSessionID || point.SealSessionID == "" {
		t.Fatalf("seal hops = %d, second hop resumed %q", len(point.SealHops), turns.specs[1].ResumeSessionID)
	}
	replayed, err := service.CreateForkPoint(context.Background(), testForkParent(cwd), ForkPointOptions{Name: "brief+ejemplos", OperationID: "op-point"})
	if err != nil || replayed.ID != point.ID || len(turns.specs) != 2 {
		t.Fatalf("replay = %+v, %v, seals=%d", replayed, err, len(turns.specs))
	}
	if _, err := service.CreateForkPoint(context.Background(), testForkParent(cwd), ForkPointOptions{Name: "other name", OperationID: "op-point"}); err == nil {
		t.Fatal("operation id reused for a different fork point was accepted")
	}
	if _, err := service.CreateForkPoint(context.Background(), testForkParent(cwd), ForkPointOptions{Name: "x", OperationID: "op-bad-effort", Effort: "turbo"}); err == nil {
		t.Fatal("an unknown effort level was accepted")
	}

	children := []ForkChildSpec{
		{Label: "frame_00 · hijo 1", Message: "review frame 00", Attachments: []string{"r01.png", "r02.png"}},
		{Label: "frame_00 · hijo 2", Message: "review frame 00 again", Attachments: []string{"r01.png"}},
		{Label: "frame_00 · hijo 3", Message: "third"},
	}
	run, err := service.StartRun(ForkRunOptions{ForkPointID: point.ID, Children: children, Concurrency: 2, OperationID: "op-run"})
	if err != nil {
		t.Fatal(err)
	}
	settledRun, settled, err := service.Wait(context.Background(), run.ID, nil, "all", 5*time.Second)
	if err != nil || !settled {
		t.Fatalf("wait settled=%v err=%v", settled, err)
	}
	for _, child := range settledRun.Children {
		if child.Status != ForkStatusCompleted || child.Usage.CacheReadTokens != 28900 || child.Turns != 2 || !strings.HasPrefix(child.Result, "done: ") {
			t.Fatalf("child = %+v", child)
		}
	}
	turns.mu.Lock()
	childSpecs := append([]forkTurnSpec(nil), turns.specs[2:]...)
	turns.mu.Unlock()
	if len(childSpecs) != 3 {
		t.Fatalf("child turns = %d", len(childSpecs))
	}
	for _, spec := range childSpecs {
		if spec.ResumeSessionID != point.SealSessionID || spec.ModeID != "bypassPermissions" || spec.Persist {
			t.Fatalf("child resumed %q mode %q, want the seal %q", spec.ResumeSessionID, spec.ModeID, point.SealSessionID)
		}
		if spec.UserText == "review frame 00" {
			// One text block (labels included) plus one block per image: label
			// blocks would push the seal out of the provider's cache lookback.
			images := 0
			for _, raw := range spec.Content {
				if raw.(map[string]any)["type"] == "image" {
					images++
				}
			}
			text, _ := spec.Content[0].(map[string]any)["text"].(string)
			if images != 2 || len(spec.Content) != 3 || !strings.Contains(text, "Attached images, in order:\n1. r01.png (852x662)\n2. r02.png (852x662)") {
				t.Fatalf("first child content = %d blocks, %d images, text %q", len(spec.Content), images, text)
			}
		}
	}
	view := ForkRunView(settledRun)
	totals := view["totals"].(map[string]any)
	if totals["cacheRead"].(int64) != 3*28900 || view["status"] != ForkStatusCompleted {
		t.Fatalf("run view = %+v", view)
	}

	// The UI can show a child's attachments and keep its transcripts.
	if attachment, data, err := service.ChildImage(run.ID, run.Children[0].ID, 1); err != nil || attachment.Name != "r02.png" || data == "" {
		t.Fatalf("child image = %+v, %d bytes, %v", attachment, len(data), err)
	}
	if _, _, err := service.ChildImage(run.ID, run.Children[0].ID, 5); err == nil {
		t.Fatal("an attachment index the child does not have was served")
	}
	if kept, err := service.SetRunRetention(run.ID, ForkRetentionKeep); err != nil || kept.Retention != ForkRetentionKeep {
		t.Fatalf("retain = %s, %v", kept.Retention, err)
	}
	again, err := service.StartRun(ForkRunOptions{ForkPointID: point.ID, Children: children, Concurrency: 2, OperationID: "op-run"})
	if err != nil || again.ID != run.ID {
		t.Fatalf("run replay = %s, %v", again.ID, err)
	}
	if _, err := service.StartRun(ForkRunOptions{ForkPointID: point.ID, Children: children[:1], OperationID: "op-run"}); err == nil {
		t.Fatal("operation id reused for a different run was accepted")
	}

	// A fresh service over the same state directory finds the receipts.
	reloaded := NewForkService(state, service.launch, nil)
	points, runs, err := reloaded.List("chat-1")
	if err != nil || len(points) != 1 || len(runs) != 1 || runs[0].Children[0].Status != ForkStatusCompleted {
		t.Fatalf("reloaded points=%d runs=%d err=%v", len(points), len(runs), err)
	}
}

func TestForkServiceRejectsBadChildrenBeforeAnyStarts(t *testing.T) {
	turns := &fakeForkTurns{}
	service, _ := newTestForkService(t, turns)
	cwd := t.TempDir()
	writeTestPNG(t, filepath.Join(cwd, "ok.png"), 100, 100)
	writeTestPNG(t, filepath.Join(cwd, "huge.png"), 1400, 900)
	point, err := service.CreateForkPoint(context.Background(), testForkParent(cwd), ForkPointOptions{Name: "p", OperationID: "op-p"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.StartRun(ForkRunOptions{ForkPointID: point.ID, OperationID: "op-bad", Children: []ForkChildSpec{
		{Message: "fine", Attachments: []string{"ok.png"}},
		{Message: "oversized", Attachments: []string{"huge.png"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "child 2") || !strings.Contains(err.Error(), "1400 px exceeds the 1000 px limit") {
		t.Fatalf("err = %v", err)
	}
	if len(turns.specs) != 2 {
		t.Fatalf("a child started despite a rejected sibling: %d turns", len(turns.specs))
	}
	if _, err := service.CreateForkPoint(context.Background(), ForkParent{ChatID: "c", TabID: "t", ProviderID: "codex", CWD: cwd, NativeSessionID: "x"}, ForkPointOptions{Name: "p", OperationID: "op-codex"}); err == nil || !strings.Contains(err.Error(), "cannot fork") {
		t.Fatalf("unsupported provider err = %v", err)
	}
}

func TestForkServiceCancelAndRetryOneChild(t *testing.T) {
	gate := make(chan struct{})
	turns := &fakeForkTurns{block: map[string]chan struct{}{"slow": gate}, started: make(chan string, 8)}
	service, _ := newTestForkService(t, turns)
	cwd := t.TempDir()
	point, err := service.CreateForkPoint(context.Background(), testForkParent(cwd), ForkPointOptions{Name: "p", OperationID: "op-p"})
	if err != nil {
		t.Fatal(err)
	}
	<-turns.started
	<-turns.started
	run, err := service.StartRun(ForkRunOptions{ForkPointID: point.ID, OperationID: "op-r", Children: []ForkChildSpec{{Message: "fast"}, {Message: "slow"}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		<-turns.started
	}
	slowID := run.Children[1].ID
	if _, err := service.CancelChild(run.ID, slowID, "op-cancel"); err != nil {
		t.Fatal(err)
	}
	settledRun, settled, err := service.Wait(context.Background(), run.ID, nil, "all", 5*time.Second)
	if err != nil || !settled {
		t.Fatalf("wait settled=%v err=%v", settled, err)
	}
	if settledRun.Children[0].Status != ForkStatusCompleted || settledRun.Children[1].Status != ForkStatusCancelled {
		t.Fatalf("statuses = %s / %s", settledRun.Children[0].Status, settledRun.Children[1].Status)
	}
	close(gate)
	retry, err := service.RetryChild(run.ID, slowID, "be quicker", "op-retry")
	if err != nil {
		t.Fatal(err)
	}
	if retry.RetryOf != slowID || retry.Attempt != 2 || !strings.Contains(retry.Message, "be quicker") {
		t.Fatalf("retry = %+v", retry)
	}
	again, err := service.RetryChild(run.ID, slowID, "be quicker", "op-retry")
	if err != nil || again.ID != retry.ID {
		t.Fatalf("retry replay = %s, %v", again.ID, err)
	}
	finalRun, settled, err := service.Wait(context.Background(), run.ID, []string{retry.ID}, "all", 5*time.Second)
	defer func() {
		summary := service.Summary()
		if len(summary) != 1 || summary[0]["chatId"] != "chat-1" || summary[0]["children"] != 2 {
			t.Fatalf("summary = %+v", summary)
		}
		if counts := summary[0]["counts"].(map[string]int); counts[ForkStatusCompleted] != 2 || counts[ForkStatusCancelled] != 0 {
			t.Fatalf("summary counts = %+v (a retried child must count once, as its latest attempt)", counts)
		}
	}()
	if err != nil || !settled {
		t.Fatalf("retry wait settled=%v err=%v", settled, err)
	}
	if got := finalRun.Children[len(finalRun.Children)-1]; got.ID != retry.ID || got.Status != ForkStatusCompleted {
		t.Fatalf("retry child = %+v", got)
	}
}

func TestForkServiceSettlesWorkInterruptedByRestart(t *testing.T) {
	state := t.TempDir()
	file := forkChatFile{
		Version: forkStoreVersion, ChatID: "chat-1", TabID: "tab-1", Operations: map[string]forkOperation{},
		Points: []ForkPoint{{ID: "fp-1", ChatID: "chat-1", Status: ForkStatusSealing}},
		Runs: []ForkRun{{ID: "fr-1", ForkPointID: "fp-1", ChatID: "chat-1", Children: []ForkChild{
			{ID: "fr-1-c1", Status: ForkStatusRunning}, {ID: "fr-1-c2", Status: ForkStatusQueued}, {ID: "fr-1-c3", Status: ForkStatusCompleted},
		}}},
	}
	data, _ := json.Marshal(file)
	path := filepath.Join(state, "forks", safeArchiveName("chat-1")+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewForkService(state, func(string) (ForkLaunch, error) { return ForkLaunch{}, nil }, nil)
	points, runs, err := service.List("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if points[0].Status != ForkStatusFailed || runs[0].Children[0].Status != ForkStatusFailed || runs[0].Children[1].Status != ForkStatusFailed || runs[0].Children[2].Status != ForkStatusCompleted {
		t.Fatalf("recovered = %+v / %+v", points[0], runs[0].Children)
	}
	if !strings.Contains(runs[0].Children[0].Error, "restarted") {
		t.Fatalf("error = %q", runs[0].Children[0].Error)
	}
}

func TestClaudeForkArgsPinTheRequestPrefix(t *testing.T) {
	args := strings.Join(claudeForkArgs(forkTurnSpec{ResumeSessionID: "seal", ModelID: "sonnet", Effort: "high", ModeID: "bypassPermissions", AddDirs: []string{"/tmp/out"}}), " ")
	for _, want := range []string{"--resume seal --fork-session", "--strict-mcp-config --mcp-config {\"mcpServers\":{}}", "--model sonnet", "--effort high", "--permission-mode bypassPermissions --allow-dangerously-skip-permissions", "--add-dir /tmp/out", "--input-format stream-json --output-format stream-json"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	if strings.Contains(strings.Join(claudeForkArgs(forkTurnSpec{ResumeSessionID: "s", ModelID: "default"}), " "), "--model") {
		t.Error("the synthetic default model must not be passed as --model")
	}
	// Throwaway children skip persistence (its per-image note blocks push the
	// fork point out of the cache lookback); seals must persist.
	if !strings.Contains(args, "--no-session-persistence") {
		t.Error("a non-persisted turn must pass --no-session-persistence")
	}
	if strings.Contains(strings.Join(claudeForkArgs(forkTurnSpec{ResumeSessionID: "s", Persist: true}), " "), "--no-session-persistence") {
		t.Error("a persisted turn (seal or kept child) must keep its session")
	}
}

// The executor runs a real process speaking Claude's stream-json contract.
func TestRunClaudeForkTurnAgainstStreamJSONProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake provider is a POSIX shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	argsFile := filepath.Join(dir, "args")
	stdinFile := filepath.Join(dir, "stdin")
	body := `#!/bin/sh
printf '%s\n' "$@" > ` + argsFile + `
cat > ` + stdinFile + `
echo '{"type":"system","subtype":"init","session_id":"22222222-2222-4222-8222-222222222222"}'
echo 'not json is ignored'
echo '{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":3,"output_tokens":1,"cache_read_input_tokens":28915,"cache_creation_input_tokens":54},"content":[{"type":"thinking","thinking":""}]}}'
echo '{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":3,"output_tokens":405,"cache_read_input_tokens":28915,"cache_creation_input_tokens":54},"content":[{"type":"tool_use","id":"t1","name":"Write","input":{"file_path":"/tmp/fork-test/child_1.json","content":"{}"}}]}}'
echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"File created"}]}}'
echo '{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":2,"output_tokens":5,"cache_read_input_tokens":57830,"cache_creation_input_tokens":9646},"content":[{"type":"text","text":"Wrote child_1.json"}]}}'
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":2,"duration_ms":4100,"result":"Wrote child_1.json","session_id":"22222222-2222-4222-8222-222222222222","usage":{"input_tokens":6,"output_tokens":410,"cache_read_input_tokens":57830,"cache_creation_input_tokens":9700}}'
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "transcript.jsonl")
	var activities []string
	result, err := claudeForkStrategy{}.RunTurn(context.Background(), forkTurnSpec{
		Launch: ForkLaunch{Command: script, Env: os.Environ()}, CWD: dir, ResumeSessionID: "seal-id", ModelID: "sonnet",
		Content: []any{map[string]any{"type": "text", "text": "review"}},
		Timeout: 10 * time.Second, TranscriptPath: transcript, UserText: "review",
		OnActivity: func(activity string) { activities = append(activities, activity) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "22222222-2222-4222-8222-222222222222" || result.Turns != 2 || result.Usage.CacheReadTokens != 57830 || result.Usage.CacheCreationTokens != 9700 || result.LastCallContext != 67478 {
		t.Fatalf("result = %+v", result)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--fork-session") || !strings.Contains(string(args), "seal-id") {
		t.Fatalf("args = %s", args)
	}
	stdin, _ := os.ReadFile(stdinFile)
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdin), &line); err != nil || line["type"] != "user" {
		t.Fatalf("stdin = %s (%v)", stdin, err)
	}
	if len(activities) == 0 || !strings.HasPrefix(activities[0], "Write /tmp/fork-test/child_1.json") {
		t.Fatalf("activities = %v", activities)
	}
	data, _ := os.ReadFile(transcript)
	// Per-call receipts keep each call's FINAL usage (the last event of a
	// multi-block call), not the first partial one.
	if !bytes.Contains(data, []byte(`"call":1,"kind":"call","usage":{"cacheCreation":54,"cacheRead":28915,"input":3}`)) ||
		!bytes.Contains(data, []byte(`"call":2,"kind":"call","usage":{"cacheCreation":9646,"cacheRead":57830,"input":2}`)) {
		t.Errorf("per-call usage missing or partial:\n%s", data)
	}
	for _, kind := range []string{`"kind":"user"`, `"kind":"tool"`, `"kind":"tool_result"`, `"kind":"assistant"`, `"kind":"result"`} {
		if !bytes.Contains(data, []byte(kind)) {
			t.Errorf("transcript missing %s", kind)
		}
	}

	// A process that never produces a result is cancelled and reported.
	hang := filepath.Join(dir, "hang")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\ncat >/dev/null\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := claudeForkStrategy{}.RunTurn(ctx, forkTurnSpec{Launch: ForkLaunch{Command: hang, Env: os.Environ()}, CWD: dir, ResumeSessionID: "s", Timeout: time.Minute})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled turn reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled turn did not stop")
	}
}

func TestRemoveNativeClaudeSessionOnlyTouchesTheExactSession(t *testing.T) {
	config := t.TempDir()
	project := filepath.Join(config, "projects", "-tmp-x")
	if err := os.MkdirAll(filepath.Join(project, "33333333-3333-4333-8333-333333333333"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"33333333-3333-4333-8333-333333333333.jsonl", "44444444-4444-4444-8444-444444444444.jsonl"} {
		if err := os.WriteFile(filepath.Join(project, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	claudeForkStrategy{}.DiscardSession(config, "../../etc")
	claudeForkStrategy{}.DiscardSession(config, "33333333-3333-4333-8333-333333333333")
	if _, err := os.Stat(filepath.Join(project, "33333333-3333-4333-8333-333333333333.jsonl")); !os.IsNotExist(err) {
		t.Fatal("throwaway session transcript was kept")
	}
	if _, err := os.Stat(filepath.Join(project, "33333333-3333-4333-8333-333333333333")); !os.IsNotExist(err) {
		t.Fatal("throwaway session sidecar was kept")
	}
	if _, err := os.Stat(filepath.Join(project, "44444444-4444-4444-8444-444444444444.jsonl")); err != nil {
		t.Fatal("an unrelated session was removed")
	}
}
