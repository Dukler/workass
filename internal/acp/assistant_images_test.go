package acp

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const assistantImageFixture = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z5m8AAAAASUVORK5CYII="

func writeAssistantImageFixture(t *testing.T, filename string) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(assistantImageFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestResolveAssistantMarkdownImagesImportsNaturalWorkspaceLinks(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	imagePath := writeAssistantImageFixture(t, filepath.Join(workspace, "calibration ready.png"))
	markdown := "[Open calibration](<" + imagePath + ">)\n![Calibration ready](<" + imagePath + ">)"

	images := ResolveAssistantMarkdownImages(markdown, workspace)
	if len(images) != 1 {
		t.Fatalf("resolved images = %d, want 1", len(images))
	}
	image := mapFromAny(images[0])
	if image["mimeType"] != "image/png" || image["data"] != assistantImageFixture {
		t.Fatalf("resolved image payload = %#v", image)
	}
	if image["name"] != "Calibration ready" || image["source"] != imagePath {
		t.Fatalf("resolved image metadata = %#v", image)
	}
}

func TestResolveAssistantMarkdownImagesRejectsNonWorkspaceAndUnsafeMedia(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	outside := writeAssistantImageFixture(t, filepath.Join(t.TempDir(), "outside.png"))
	insideSVG := filepath.Join(workspace, "active.svg")
	if err := os.WriteFile(insideSVG, []byte("<svg></svg>"), 0o600); err != nil {
		t.Fatal(err)
	}
	markdown := strings.Join([]string{
		"![Outside](" + outside + ")",
		"![Remote](https://example.invalid/remote.png)",
		"![Active](" + insideSVG + ")",
	}, "\n")
	if images := ResolveAssistantMarkdownImages(markdown, workspace); len(images) != 0 {
		t.Fatalf("unsafe assistant images were imported: %#v", images)
	}
}

func TestJobPublicCarriesBoundedAssistantImages(t *testing.T) {
	t.Parallel()
	job := &Job{ID: "assistant-media-job"}
	job.addAssistantImages([]any{
		map[string]any{"mimeType": "image/png", "data": assistantImageFixture, "name": "Preview", "source": "preview.png"},
		map[string]any{"mimeType": "image/png", "data": assistantImageFixture, "name": "Duplicate", "source": "preview.png"},
	})
	images, _ := job.Public()["images"].([]any)
	if len(images) != 1 {
		t.Fatalf("public assistant images = %#v, want one deduplicated attachment", images)
	}
}

func TestAgentMessageChunkPreservesStructuredACPImages(t *testing.T) {
	t.Parallel()
	events := newEventCollector()
	mgr := NewManager(Options{Broadcast: events.Broadcast})
	t.Cleanup(func() { mgr.Reset() })
	bridge := &Bridge{
		manager:       mgr,
		opts:          Options{StdoutFlushInterval: time.Hour},
		jobsBySession: map[string]*Job{},
	}
	job := &Job{ID: "structured-assistant-image"}
	bridge.jobsBySession["structured-session"] = job
	bridge.handleNotification("session/update", map[string]any{
		"sessionId": "structured-session",
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content": []any{
				map[string]any{"type": "text", "text": "preview"},
				map[string]any{"type": "image", "mimeType": "image/png", "data": assistantImageFixture, "name": "Structured preview"},
			},
		},
	})
	images, _ := job.Public()["images"].([]any)
	if len(images) != 1 || mapFromAny(images[0])["name"] != "Structured preview" {
		t.Fatalf("structured assistant images = %#v", images)
	}
	live := events.waitJobType(t, job.ID, "assistant-media", time.Second)
	liveImages, _ := live["images"].([]any)
	if len(liveImages) != 1 || mapFromAny(liveImages[0])["name"] != "Structured preview" {
		t.Fatalf("live structured assistant images = %#v", liveImages)
	}
	bridge.flushJobBuffers(job)
}

func TestAgentMessageMarkdownImageStreamsBeforeTurnEndAcrossSplitChunks(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	imagePath := writeAssistantImageFixture(t, filepath.Join(workspace, "recording started.png"))
	events := newEventCollector()
	mgr := NewManager(Options{Broadcast: events.Broadcast})
	t.Cleanup(func() { mgr.Reset() })
	bridge := &Bridge{
		manager:       mgr,
		opts:          Options{StdoutFlushInterval: time.Hour},
		jobsBySession: map[string]*Job{},
	}
	job := &Job{ID: "streaming-markdown-image", Status: "running", CWD: workspace}
	bridge.jobsBySession["streaming-markdown-session"] = job
	cut := len(imagePath) / 2
	for _, text := range []string{
		"Recording is active.\n![Recording started](" + imagePath[:cut],
		imagePath[cut:] + ")\nKeep moving while the turn remains active.",
	} {
		bridge.handleNotification("session/update", map[string]any{
			"sessionId": "streaming-markdown-session",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       []any{map[string]any{"type": "text", "text": text}},
			},
		})
	}

	live := events.waitJobType(t, job.ID, "assistant-media", time.Second)
	liveImages, _ := live["images"].([]any)
	if len(liveImages) != 1 {
		t.Fatalf("live Markdown assistant images = %#v, want one before terminal", liveImages)
	}
	image := mapFromAny(liveImages[0])
	if image["source"] != imagePath || image["name"] != "Recording started" {
		t.Fatalf("live Markdown assistant image = %#v", image)
	}
	if job.Status != "running" {
		t.Fatalf("media import changed active job status to %q", job.Status)
	}
	dataIndex, mediaIndex := -1, -1
	for index, event := range events.snapshot() {
		if event.channel != "job:event" {
			continue
		}
		payload := mapFromAny(event.payload)
		if asString(payload["id"]) != job.ID {
			continue
		}
		switch asString(payload["type"]) {
		case "data":
			if dataIndex < 0 {
				dataIndex = index
			}
		case "assistant-media":
			mediaIndex = index
		}
	}
	if dataIndex < 0 || mediaIndex < 0 || dataIndex >= mediaIndex {
		t.Fatalf("live text/media order = data:%d media:%d, want authored text before resolving media", dataIndex, mediaIndex)
	}
	bridge.flushJobBuffers(job)
}

func TestMockNaturalAssistantImageCompletesAsDurableJobMedia(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	workspace := t.TempDir()
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Provider: ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Label: "Workass Mock ACP", Enabled: true,
		},
		DefaultProviderID:   "mock",
		Broadcast:           events.Broadcast,
		StdoutFlushInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "assistant-image-tab", ChatID: "assistant-image-chat", CWD: workspace, ProviderID: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "assistant-image-tab", ChatID: "assistant-image-chat",
		CWD: workspace, ProviderID: "mock", Prompt: "[mock:assistant-image]",
	})
	if err != nil {
		t.Fatal(err)
	}
	end := events.waitJobEnd(t, jobID(job), 5*time.Second)
	allEvents := events.snapshot()
	mediaIndex, endIndex := -1, -1
	for index, event := range allEvents {
		if event.channel != "job:event" {
			continue
		}
		payload := mapFromAny(event.payload)
		if asString(payload["id"]) == jobID(job) && asString(payload["type"]) == "assistant-media" {
			mediaIndex = index
		}
		if asString(payload["type"]) == "end" && asString(mapFromAny(payload["job"])["id"]) == jobID(job) {
			endIndex = index
		}
	}
	if mediaIndex < 0 || endIndex < 0 || mediaIndex >= endIndex {
		t.Fatalf("assistant media/end order = media:%d end:%d, want live media before terminal", mediaIndex, endIndex)
	}
	endJob := jobFromEnd(end)
	images, _ := endJob["images"].([]any)
	if len(images) != 1 {
		t.Fatalf("terminal mock assistant images = %#v", images)
	}
	image := mapFromAny(images[0])
	if image["mimeType"] != "image/png" || image["name"] != "Deterministic preview" {
		t.Fatalf("terminal mock assistant image = %#v", image)
	}
	source := asString(image["source"])
	if source == "" || filepath.Dir(source) != workspace {
		t.Fatalf("terminal mock assistant source escaped workspace: %q", source)
	}
}

func TestIncrementalAssistantImageParserMatchesWholeAnswerAcrossSplits(t *testing.T) {
	fixtures := []string{
		"prose ![First](a.png) then ![Second](<b c.png>) final",
		"```markdown\n![hidden](x.png)\n```\n![shown](yes.png)",
		"  ~~~\n![hidden](x.png)\n  ~~~\n![shown](yes.png)",
		"\u00a0```\n![hidden](x.png)\n\u2003```\n![shown](yes.png)",
		"`![hidden](x.png)` and ![shown](yes.png)",
		"![unfinished\nnext ![complete](y.png)",
		"![empty]() and ![unclosed](<bad\n![good](z.png)",
		"![label ] x]( path.png ) and ![other](<x)y.png>)",
		"\\` literal ![visible](a.png)",
	}
	for _, source := range fixtures {
		want := assistantMarkdownImageRefs(source)
		for size := 1; size <= len(source); size++ {
			var scanner assistantMarkdownScanner
			var got []assistantMarkdownImageRef
			for start := 0; start < len(source); start += size {
				got = append(got, scanner.feed(source[start:min(start+size, len(source))])...)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("chunk size %d: got %+v, want %+v for %q", size, got, want, source)
			}
		}
	}
}

func TestAssistantImagesDoNotRereadImportedFilesAndRetryMissingAtTerminal(t *testing.T) {
	cwd := t.TempDir()
	path := writeAssistantImageFixture(t, filepath.Join(cwd, "first.png"))
	job := &Job{CWD: cwd}
	first := []assistantMarkdownImageRef{{name: "First", source: path}}
	job.addAssistantImages(job.resolveAssistantMarkdownImages(first, false))
	if len(job.assistantImagesSnapshot()) != 1 {
		t.Fatal("initial image was not captured")
	}
	// Keep the file readable: an uncached resolver would return it again.
	if got := job.resolveAssistantMarkdownImages(first, true); len(got) != 0 {
		t.Fatal("terminal reread an imported source")
	}
	missing := filepath.Join(cwd, "later.png")
	later := []assistantMarkdownImageRef{{name: "Later", source: missing}}
	if got := job.resolveAssistantMarkdownImages(later, false); len(got) != 0 {
		t.Fatal("missing image unexpectedly resolved")
	}
	writeAssistantImageFixture(t, missing)
	if got := job.resolveAssistantMarkdownImages(later, false); len(got) != 0 {
		t.Fatal("streaming retried an already attempted path")
	}
	if got := job.resolveAssistantMarkdownImages(later, true); len(got) != 1 {
		t.Fatal("terminal did not retry a newly available file")
	}
}

func TestIncompleteImageDoesNotRescanOutputOrResolveFilesPerChunk(t *testing.T) {
	mgr := NewManager(Options{})
	t.Cleanup(func() { mgr.Reset() })
	bridge := &Bridge{manager: mgr, opts: Options{StdoutFlushInterval: time.Hour}}
	job := &Job{ID: "incremental-image", CWD: t.TempDir()}
	bridge.queueStdout(job, strings.Repeat("prefix ", 10000)+"![unfinished", "")
	for i := 0; i < 1000; i++ {
		bridge.queueStdout(job, "x", "")
	}
	bridge.flushJobBuffers(job)
	stats := mapFromAny(mgr.Stats()["stream"])
	if stats["markdownScanBytes"] != stats["chunkBytes"] {
		t.Fatal("streaming rescanned bytes from earlier chunks")
	}
	if stats["imageResolves"] != uint64(0) {
		t.Fatal("incomplete image triggered filesystem resolution")
	}
}
