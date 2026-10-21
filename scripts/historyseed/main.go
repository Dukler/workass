// Command historyseed grows mock-provider chats in a scratch state directory
// into large, realistic histories for load measurement. Each target chat must
// already exist with one real mock turn (its lane, thread and coverage are
// genuine); the seeder clones that turn's shape thousands of times with big
// tool outputs, thinking, markdown, content-addressed images and a long
// subagent run. It writes through chat.FileStore, so the result is exactly
// the on-disk format the daemon reads. Never point it at a real state dir.
//
// Recipe: run a scratch dev daemon (mock provider, its own --state-dir), make
// each chat with one "[mock:tool-image] [mock:assistant-image]" turn through
// the UI or window.api, stop the daemon, then grow them:
//
//	go run ./scripts/historyseed -state /tmp/wk-load/state -chat seed-huge-0-chat=3000 -chat seed-medium-0-chat=200
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"workass/internal/chat"
	"workass/internal/provider"
)

type chatTarget struct {
	chatID   string
	messages int
}

type targets []chatTarget

func (t *targets) String() string { return fmt.Sprint(*t) }
func (t *targets) Set(value string) error {
	id, count, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("want chatId=messages, got %q", value)
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 2 {
		return fmt.Errorf("bad message count %q", count)
	}
	*t = append(*t, chatTarget{chatID: strings.TrimSpace(id), messages: n})
	return nil
}

func main() {
	stateDir := flag.String("state", "", "scratch daemon state directory (never a real one)")
	images := flag.Int("images", 12, "distinct generated images")
	imageEvery := flag.Int("image-every", 25, "add a tool image and an assistant image every N turns (0 = never)")
	subagentTools := flag.Int("subagent-tools", 400, "tool rows in the one long subagent run of each chat over 400 messages")
	seed := flag.Int64("seed", 1, "deterministic content seed")
	var chats targets
	flag.Var(&chats, "chat", "chatId=messages (repeatable)")
	flag.Parse()
	if strings.TrimSpace(*stateDir) == "" || len(chats) == 0 {
		fmt.Fprintln(os.Stderr, "historyseed: -state and at least one -chat are required")
		os.Exit(2)
	}
	if strings.Contains(*stateDir, "Library/Application Support/Workass") {
		fmt.Fprintln(os.Stderr, "historyseed: refusing to write a real Workass state dir")
		os.Exit(2)
	}
	refs, err := writeImages(*stateDir, *images, *seed)
	if err != nil {
		fail(err)
	}
	for index, target := range chats {
		started := time.Now()
		if err := grow(*stateDir, target, refs, *imageEvery, *subagentTools, *seed+int64(index)); err != nil {
			fail(fmt.Errorf("%s: %w", target.chatID, err))
		}
		info, _ := os.Stat(actorPath(*stateDir, target.chatID))
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		fmt.Printf("seeded %s: %d messages, actor %.1f MB, %s\n", target.chatID, target.messages, float64(size)/(1<<20), time.Since(started).Round(time.Millisecond))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "historyseed:", err)
	os.Exit(1)
}

func actorPath(stateDir, chatID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(chatID)))
	return filepath.Join(stateDir, "provider-chats", hex.EncodeToString(digest[:])+".json")
}

// writeImages stores noise PNGs the way the daemon does: the file holds the
// base64 payload and is named by the sha256 of that payload.
func writeImages(stateDir string, count int, seed int64) ([]string, error) {
	random := rand.New(rand.NewSource(seed))
	dir := filepath.Join(stateDir, "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	refs := make([]string, 0, count)
	for index := 0; index < count; index++ {
		width, height := 640, 360
		img := image.NewNRGBA(image.Rect(0, 0, width, height))
		base := color.NRGBA{uint8(random.Intn(255)), uint8(random.Intn(255)), uint8(random.Intn(255)), 255}
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				noise := uint8(random.Intn(64))
				img.SetNRGBA(x, y, color.NRGBA{base.R ^ noise, base.G ^ uint8(x), base.B ^ uint8(y), 255})
			}
		}
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, img); err != nil {
			return nil, err
		}
		data := base64.StdEncoding.EncodeToString(encoded.Bytes())
		sum := sha256.Sum256([]byte(data))
		name := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			return nil, err
		}
		refs = append(refs, "workass-session-image:images/"+name)
	}
	return refs, nil
}

func grow(stateDir string, target chatTarget, imageRefs []string, imageEvery, subagentTools int, seed int64) error {
	store := chat.FileStore{Path: actorPath(stateDir, target.chatID)}
	state, ok, err := store.Load(target.chatID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("actor not found; create it with one real mock turn first")
	}
	if state.Foreground != nil || len(state.Ledger) < 2 {
		return fmt.Errorf("actor must be idle with one finished template turn")
	}
	var templateUser, templateAssistant *chat.LedgerEvent
	for index := range state.Ledger {
		event := &state.Ledger[index]
		if event.Role == "user" && templateUser == nil {
			templateUser = event
		}
		if event.Role == "assistant" && templateAssistant == nil {
			templateAssistant = event
		}
	}
	if templateUser == nil || templateAssistant == nil {
		return fmt.Errorf("template turn is missing a user or assistant row")
	}
	random := rand.New(rand.NewSource(seed))
	// The template turn stays first: provider activity, tools and receipts are
	// owned by its exact operation.
	turns := (target.messages - len(state.Ledger)) / 2
	start := time.Now().Add(-time.Duration(turns) * 3 * time.Minute).UTC()
	ledger := append(make([]chat.LedgerEvent, 0, len(state.Ledger)+turns*2), state.Ledger...)
	subagentTurn := -1
	if target.messages > 400 {
		subagentTurn = turns / 3
	}
	for turn := 0; turn < turns; turn++ {
		at := start.Add(time.Duration(turn) * 3 * time.Minute)
		operation := provider.OperationID(fmt.Sprintf("%s-seed-op-%05d", target.chatID, turn))
		user := *templateUser
		user.EventID = fmt.Sprintf("event:%s:user", operation)
		user.MessageID = fmt.Sprintf("%s-seed-u-%05d", target.chatID, turn)
		user.OperationID = operation
		user.Text = fmt.Sprintf("Turn %d. %s", turn, prose(random, 60+random.Intn(500)))
		user.At = at.Format(time.RFC3339Nano)
		user.NativeTurnID = fmt.Sprintf("app-chat-seed-%s-%05d", target.chatID, turn)
		user.Attachments = nil
		assistant := *templateAssistant
		assistant.EventID = fmt.Sprintf("event:%s:assistant", operation)
		assistant.MessageID = fmt.Sprintf("%s-seed-a-%05d", target.chatID, turn)
		assistant.OperationID = operation
		assistant.NativeTurnID = user.NativeTurnID
		assistant.At = at.Add(40 * time.Second).Format(time.RFC3339Nano)
		assistant.TurnStartedAt = at.UnixMilli()
		assistant.Attachments = nil
		assistant.Terminal = nil
		assistant.Text = markdown(random, turn)
		assistant.Result = ""
		assistant.Timeline = timeline(random, turn, imageRefs, imageEvery, turn == subagentTurn, subagentTools, at)
		if imageEvery > 0 && turn%imageEvery == imageEvery/2 && len(imageRefs) > 0 {
			ref := imageRefs[(turn/imageEvery+3)%len(imageRefs)]
			assistant.Attachments = []provider.Attachment{{Name: "Captura", MIMEType: "image/png", Ref: ref}}
		}
		ledger = append(ledger, user, assistant)
	}
	for index := range ledger {
		ledger[index].Sequence = uint64(index + 1)
	}
	state.Ledger = ledger
	for laneID, lane := range state.Lanes {
		lane.Coverage = make(map[uint64]chat.CoverageRecord, len(ledger))
		for _, event := range ledger {
			if event.LaneID != laneID {
				continue
			}
			lane.Coverage[event.Sequence] = chat.CoverageRecord{
				Sequence: event.Sequence, EventID: event.EventID, Status: chat.CoverageNativeSeen, DeliveryID: event.OperationID,
			}
		}
		covered := uint64(0)
		for sequence := uint64(1); ; sequence++ {
			if _, ok := lane.Coverage[sequence]; !ok {
				break
			}
			covered = sequence
		}
		lane.CoveredThrough = covered
		state.Lanes[laneID] = lane
	}
	state.Revision++
	if err := state.Validate(); err != nil {
		return err
	}
	if err := store.Save(state); err != nil {
		return err
	}
	_ = os.Remove(store.Path + ".events")
	return nil
}

var words = strings.Fields(`the daemon renders history page actor ledger stream reply tool output image chat window
scroll anchor bottom pinned virtual list message turn provider lane thread resume cache switch paint frame
budget bounded snapshot cursor older newer row height measure layout fold expand inline workspace build test`)

func prose(random *rand.Rand, chars int) string {
	var b strings.Builder
	for b.Len() < chars {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(words[random.Intn(len(words))])
		if random.Intn(14) == 0 {
			b.WriteString(".")
		}
	}
	return b.String()
}

func markdown(random *rand.Rand, turn int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Step %d\n\n", turn)
	paragraphs := 1 + random.Intn(5)
	for p := 0; p < paragraphs; p++ {
		b.WriteString(prose(random, 120+random.Intn(700)))
		b.WriteString("\n\n")
		if random.Intn(3) == 0 {
			for item := 0; item < 2+random.Intn(5); item++ {
				fmt.Fprintf(&b, "- %s\n", prose(random, 30+random.Intn(90)))
			}
			b.WriteString("\n")
		}
		if random.Intn(4) == 0 {
			b.WriteString("```go\n")
			for line := 0; line < 4+random.Intn(30); line++ {
				fmt.Fprintf(&b, "func step%d_%d(x int) int { return x * %d } // %s\n", turn, line, line, prose(random, 20))
			}
			b.WriteString("```\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func toolOutput(random *rand.Rand, size int) string {
	var b strings.Builder
	line := 0
	for b.Len() < size {
		fmt.Fprintf(&b, "%05d  %s\n", line, prose(random, 40+random.Intn(80)))
		line++
	}
	return b.String()
}

func timeline(random *rand.Rand, turn int, imageRefs []string, imageEvery int, subagent bool, subagentTools int, at time.Time) []chat.TimelineEntry {
	entries := []chat.TimelineEntry{{
		Key: fmt.Sprintf("thinking-%d", turn), At: 0, Kind: provider.EventThinkingUpdate,
		Thinking: &provider.ThinkingEvent{Text: prose(random, 200+random.Intn(1800))},
	}}
	tools := random.Intn(9)
	for index := 0; index < tools; index++ {
		size := 200 + random.Intn(4000)
		switch random.Intn(10) {
		case 0:
			size = 100_000 + random.Intn(150_000)
		case 1, 2:
			size = 10_000 + random.Intn(40_000)
		}
		tool := &provider.ToolEvent{
			ToolCallID: fmt.Sprintf("call-%d-%d", turn, index), ToolKind: "execute", Title: fmt.Sprintf("go test ./... (%d)", index),
			Status: "completed", Command: fmt.Sprintf("go test ./internal/... -run Step%d", index),
			Input: `{"command":"go test ./..."}`, Output: toolOutput(random, size),
			StartedAtUnixMS: at.UnixMilli() + int64(index*1000), EndedAtUnixMS: at.UnixMilli() + int64(index*1000+800),
		}
		if index == 0 && imageEvery > 0 && turn%imageEvery == 0 && len(imageRefs) > 0 {
			tool.Attachments = []provider.Attachment{{Name: "Tool image", MIMEType: "image/png", Ref: imageRefs[(turn/imageEvery)%len(imageRefs)]}}
		}
		entries = append(entries, chat.TimelineEntry{Key: tool.ToolCallID, At: 0, Kind: provider.EventToolUpdate, Tool: tool})
	}
	if subagent {
		header := &provider.ToolEvent{
			ToolCallID: fmt.Sprintf("subagent-%d", turn), ToolKind: "think", Title: "Explore: audit the timeline",
			Status: "completed", SubagentID: "sub-seed-1", SubagentLabel: "Explore", SubagentProvider: "mock",
			SubagentModel: "mock-deterministic", SubagentHeader: true, Input: "Audit every timeline row.",
			Output: prose(random, 3000), StartedAtUnixMS: at.UnixMilli(), EndedAtUnixMS: at.Add(50 * time.Minute).UnixMilli(),
		}
		entries = append(entries, chat.TimelineEntry{Key: header.ToolCallID, Kind: provider.EventToolUpdate, Tool: header})
		for index := 0; index < subagentTools; index++ {
			tool := &provider.ToolEvent{
				ToolCallID: fmt.Sprintf("subagent-%d-call-%d", turn, index), ToolKind: "read", Title: fmt.Sprintf("Read internal/file_%d.go", index),
				Status: "completed", SubagentID: "sub-seed-1", SubagentLabel: "Explore", Location: fmt.Sprintf("internal/file_%d.go", index),
				Output: toolOutput(random, 500+random.Intn(6000)), StartedAtUnixMS: at.UnixMilli() + int64(index*7000),
				EndedAtUnixMS: at.UnixMilli() + int64(index*7000+3000),
			}
			entries = append(entries, chat.TimelineEntry{Key: tool.ToolCallID, Kind: provider.EventToolUpdate, Tool: tool})
		}
	}
	return entries
}
