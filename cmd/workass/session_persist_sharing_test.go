package main

import (
	"encoding/base64"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workass/internal/acp"
)

// The persist path and the redactor both share structure with the live mirror
// now, so the tests that matter are "the live snapshot is never mutated" and
// "sharing produced the same bytes a full rebuild would have".

func testImagePNG(seed string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(seed, 64)))
}

func snapshotWithImages() map[string]any {
	shared := testImagePNG("ab")
	return map[string]any{
		"chats": []any{map[string]any{
			"id": "tab-1", "chatId": "chat-1", "title": "with images",
			"messages": []any{
				map[string]any{
					"id": "m1", "role": "user", "content": "look at this",
					"images": []any{
						map[string]any{"mimeType": "image/png", "name": "shot.png", "data": shared},
					},
					"events": []any{
						// Byte-identical to the message image, name included: dedupe keys on
						// the whole node, so only an exact match becomes an index ref.
						map[string]any{"kind": "tool", "images": []any{
							map[string]any{"mimeType": "image/png", "name": "shot.png", "data": shared},
						}},
						// Distinct payload: must be externalized, not deduped.
						map[string]any{"kind": "tool", "images": []any{
							map[string]any{"mimeType": "image/png", "data": testImagePNG("cd")},
						}},
						map[string]any{"kind": "thinking", "text": "no images here"},
					},
				},
				map[string]any{"id": "m2", "role": "assistant", "content": "plain turn, no images"},
			},
		}},
	}
}

func TestPreparePersistenceNeverMutatesLiveSnapshot(t *testing.T) {
	live := snapshotWithImages()
	reference, err := json.Marshal(live)
	if err != nil {
		t.Fatalf("marshal reference: %v", err)
	}

	dir := t.TempDir()
	out, err := prepareSessionSnapshotForPersistence(live, dir)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	after, err := json.Marshal(live)
	if err != nil {
		t.Fatalf("marshal after: %v", err)
	}
	if string(reference) != string(after) {
		t.Fatalf("prepareSessionSnapshotForPersistence mutated the live snapshot\nbefore: %s\nafter:  %s", reference, after)
	}

	// And the persisted form must still be fully externalized.
	persisted, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal persisted: %v", err)
	}
	if strings.Contains(string(persisted), testImagePNG("ab")) || strings.Contains(string(persisted), testImagePNG("cd")) {
		t.Fatal("persisted snapshot still carries inline base64")
	}
	if !strings.Contains(string(persisted), sessionImageDataRefField) {
		t.Fatalf("persisted snapshot has no image ref: %s", persisted)
	}
	if !strings.Contains(string(persisted), sessionMessageImageRefField) {
		t.Fatalf("duplicate event image was not deduped to a message ref: %s", persisted)
	}
}

func TestPreparePersistenceRoundTripsThroughRehydration(t *testing.T) {
	dir := t.TempDir()
	live := snapshotWithImages()
	out, err := prepareSessionSnapshotForPersistence(live, dir)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// The images directory sits next to the state file, which is what both the
	// persist and the load path derive from.
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded map[string]any
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	statePath := filepath.Join(dir, sessionStateFilename)
	if err := rehydrateExternalSessionImages(reloaded, filepath.Dir(statePath)); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if err := rehydrateSessionEventImageRefs(reloaded); err != nil {
		t.Fatalf("rehydrate event refs: %v", err)
	}
	got, err := json.Marshal(reloaded)
	if err != nil {
		t.Fatalf("marshal reloaded: %v", err)
	}
	want, err := json.Marshal(snapshotWithImages())
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("persist/rehydrate round trip lost fidelity\nwant: %s\ngot:  %s", want, got)
	}
}

// redactSessionValueRebuilt is the pre-optimization redactor: it always rebuilds
// every node. The optimized version must agree with it exactly. String-level
// scrubbing is shared with the real implementation; the prefilter that skips the
// regex is proven equivalent in internal/acp.
func redactSessionValueRebuilt(raw any) any {
	switch value := raw.(type) {
	case string:
		return acp.RedactSensitiveText(value)
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			if secretKeyRE.MatchString(key) {
				out[key] = "[redacted]"
			} else {
				out[key] = redactSessionValueRebuilt(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = redactSessionValueRebuilt(item)
		}
		return out
	default:
		return value
	}
}

func TestRedactSessionValueSharingMatchesRebuild(t *testing.T) {
	cases := []any{
		snapshotWithImages(),
		map[string]any{"authorization": "Bearer sk-abcdef0123456789", "plain": "nothing here"},
		map[string]any{"nested": []any{
			map[string]any{"cmd": "export API_KEY=supersecretvalue && run"},
			map[string]any{"cmd": "echo hello"},
			"token: abc123",
			"password=hunter2",
			"credential : xyz",
			"BEARER   ZZZ999",
			"a bear ate my key",
			"keystone arch",
			"",
		}},
		map[string]any{"apiKey": "literal-key-name-is-redacted", "empty": map[string]any{}, "list": []any{}},
		[]any{"secret=1", "fine", map[string]any{"deep": map[string]any{"deeper": "no secrets"}}},
	}
	for i, input := range cases {
		want, err := json.Marshal(redactSessionValueRebuilt(input))
		if err != nil {
			t.Fatalf("case %d marshal want: %v", i, err)
		}
		got, err := json.Marshal(redactSessionValue(input))
		if err != nil {
			t.Fatalf("case %d marshal got: %v", i, err)
		}
		if string(want) != string(got) {
			t.Fatalf("case %d: structural-sharing redactor disagreed with rebuild\nwant: %s\ngot:  %s", i, want, got)
		}
	}
}

// Save mutates the tree redactSessionValue hands back (it strips _workassSave and
// rewrites chats), so the redactor must never share nodes with its caller. An
// earlier revision shared untouched subtrees and silently stripped the save mode
// from the renderer's own payload.
func TestRedactSessionValueNeverSharesWithCaller(t *testing.T) {
	clean := map[string]any{"a": []any{map[string]any{"b": "nothing sensitive"}}}
	out := redactSessionValue(clean)
	if reflect.ValueOf(out).Pointer() == reflect.ValueOf(clean).Pointer() {
		t.Fatal("redactSessionValue returned the caller's own root map")
	}
	inner := mapFromAnyMain(anySlice(mapFromAnyMain(out)["a"])[0])
	originalInner := mapFromAnyMain(anySlice(clean["a"])[0])
	if reflect.ValueOf(inner).Pointer() == reflect.ValueOf(originalInner).Pointer() {
		t.Fatal("redactSessionValue shared a nested map with the caller")
	}
	// Mutating the result must leave the caller's tree alone.
	inner["b"] = "changed"
	delete(mapFromAnyMain(out), "a")
	if got := fieldString(originalInner, "b"); got != "nothing sensitive" {
		t.Fatalf("caller's tree was mutated through the redacted result: %q", got)
	}
	if _, present := clean["a"]; !present {
		t.Fatal("deleting a key on the result removed it from the caller's map")
	}

	dirty := map[string]any{"keep": "fine", "inner": map[string]any{"cmd": "token=abc"}}
	redacted := mapFromAnyMain(redactSessionValue(dirty))
	if reflect.ValueOf(redacted).Pointer() == reflect.ValueOf(dirty).Pointer() {
		t.Fatal("a node containing a secret must be rebuilt, not shared")
	}
	if got := fieldString(mapFromAnyMain(dirty["inner"]), "cmd"); got != "token=abc" {
		t.Fatalf("redaction mutated the caller's tree: cmd = %q", got)
	}
	if got := fieldString(mapFromAnyMain(redacted["inner"]), "cmd"); got != "token=[redacted]" {
		t.Fatalf("nested secret survived: %q", got)
	}
}

// The persist path is the one that runs inside the mutex, so there it does share
// every node it will not rewrite. Copying the whole mirror there cost 275 ms per
// save in front of the streaming path.
func TestCloneSessionImagePathsSharesEverythingButImages(t *testing.T) {
	live := snapshotWithImages()
	out := mapFromAnyMain(cloneSessionImagePaths(live))
	if reflect.ValueOf(out).Pointer() == reflect.ValueOf(live).Pointer() {
		t.Fatal("root must be copied: it is on the path to an image")
	}

	chat := mapFromAnyMain(anySlice(out["chats"])[0])
	liveChat := mapFromAnyMain(anySlice(live["chats"])[0])
	messages, liveMessages := messageSlice(chat), messageSlice(liveChat)

	// The image-bearing message is copied...
	withImages, liveWithImages := mapFromAnyMain(messages[0]), mapFromAnyMain(liveMessages[0])
	if reflect.ValueOf(withImages).Pointer() == reflect.ValueOf(liveWithImages).Pointer() {
		t.Fatal("a message holding an inline image must be copied")
	}
	// ...and the plain turn beside it is shared.
	plain, livePlain := mapFromAnyMain(messages[1]), mapFromAnyMain(liveMessages[1])
	if reflect.ValueOf(plain).Pointer() != reflect.ValueOf(livePlain).Pointer() {
		t.Fatal("a message with no images was copied; sharing is what makes this cheap")
	}
	// So is an event with no images, inside a message that does have one.
	events, liveEvents := anySlice(withImages["events"]), anySlice(liveWithImages["events"])
	thinking, liveThinking := mapFromAnyMain(events[2]), mapFromAnyMain(liveEvents[2])
	if reflect.ValueOf(thinking).Pointer() != reflect.ValueOf(liveThinking).Pointer() {
		t.Fatal("an event with no images was copied")
	}

	// A snapshot with no inline images at all must be returned verbatim.
	imageless := map[string]any{"chats": []any{map[string]any{"id": "t", "messages": []any{
		map[string]any{"id": "m", "content": "text only"},
	}}}}
	if reflect.ValueOf(cloneSessionImagePaths(imageless)).Pointer() != reflect.ValueOf(imageless).Pointer() {
		t.Fatal("an image-free snapshot was copied instead of shared")
	}
}

func TestSessionGenerationsShareImmutableHeavyPayloadsOnly(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(statePath)
	initial := snapshotWithImages()
	message := mapFromAnyMain(messageSlice(mapFromAnyMain(anySlice(initial["chats"])[0]))[0])
	firstTool := mapFromAnyMain(anySlice(message["events"])[0])
	firstTool["id"] = "tool-1"
	firstTool["output"] = map[string]any{
		"content": strings.Repeat("large immutable tool output", 4096),
		"meta":    map[string]any{"kind": "result"},
	}
	type capturedGeneration struct {
		generation *sessionGeneration
		message    map[string]any
		tool       map[string]any
		images     []any
		output     map[string]any
	}
	var captured capturedGeneration
	capturePublication := func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		generation := store.generation
		if generation == nil || generation.root == nil {
			t.Fatal("generation root was not published for outside-lock marshal")
		}
		if generation.chatsByTab != nil || generation.messagesByTab != nil {
			t.Fatal("published generation built Save-only indexes")
		}
		publishedMessage := mapFromAnyMain(messageSlice(chatFromSnapshot(generation.root, "tab-1"))[0])
		publishedTool := mapFromAnyMain(anySlice(publishedMessage["events"])[0])
		captured = capturedGeneration{
			generation: generation,
			message:    publishedMessage,
			tool:       publishedTool,
			images:     anySlice(publishedMessage["images"]),
			output:     mapFromAnyMain(publishedTool["output"]),
		}
	}
	store.beforeGenerationMarshal = capturePublication
	if !store.Save(initial) {
		t.Fatal("initial save")
	}
	firstGeneration := captured
	if firstGeneration.generation == nil {
		t.Fatal("initial save did not publish a generation")
	}
	store.mu.Lock()
	if store.generation.root == nil {
		store.mu.Unlock()
		t.Fatal("completed save retired the sole published root")
	}
	if reflect.ValueOf(store.generation.root).Pointer() != reflect.ValueOf(store.snapshot).Pointer() {
		store.mu.Unlock()
		t.Fatal("completed save retained a second structural mirror root")
	}
	store.mu.Unlock()

	lean := store.Get().(map[string]any)
	lean["_workassSave"] = "lean-payload-v2"
	lean["_workassDeletedChatIds"] = []any{}
	leanMessage := mapFromAnyMain(messageSlice(mapFromAnyMain(anySlice(lean["chats"])[0]))[0])
	delete(leanMessage, "images")
	leanMessage["events"] = []any{map[string]any{
		"kind": "tool", "id": "tool-1", "startedAt": "2026-07-24T00:00:00Z",
	}}
	if !store.Save(lean) {
		t.Fatal("lean save")
	}
	secondGeneration := captured
	if secondGeneration.generation == nil ||
		secondGeneration.generation.number <= firstGeneration.generation.number ||
		secondGeneration.generation.persistenceSeq <= firstGeneration.generation.persistenceSeq {
		t.Fatalf("generation did not advance: first=%#v second=%#v", firstGeneration, secondGeneration)
	}
	store.mu.Lock()
	if store.generation.root == nil {
		store.mu.Unlock()
		t.Fatal("latest completed save retired its published root")
	}
	if reflect.ValueOf(store.generation.root).Pointer() != reflect.ValueOf(store.snapshot).Pointer() {
		store.mu.Unlock()
		t.Fatal("latest completed save retained a second structural mirror root")
	}
	store.mu.Unlock()

	firstMessage := firstGeneration.message
	secondMessage := secondGeneration.message
	firstImages := firstGeneration.images
	secondImages := secondGeneration.images
	if reflect.ValueOf(firstImages).Pointer() != reflect.ValueOf(secondImages).Pointer() {
		t.Fatal("unchanged ref-only image array was copied between immutable generations")
	}
	findTool := func(message map[string]any, id string) map[string]any {
		for _, raw := range anySlice(message["events"]) {
			event := mapFromAnyMain(raw)
			if fieldString(event, "id") == id {
				return event
			}
		}
		return nil
	}
	firstGenerationTool := findTool(firstMessage, "tool-1")
	secondGenerationTool := findTool(secondMessage, "tool-1")
	if reflect.ValueOf(firstGenerationTool).Pointer() == reflect.ValueOf(secondGenerationTool).Pointer() {
		t.Fatal("overlay metadata mutated a shared tool-event map")
	}
	firstOutput := firstGeneration.output
	secondOutput := secondGeneration.output
	if reflect.ValueOf(firstOutput).Pointer() != reflect.ValueOf(secondOutput).Pointer() {
		t.Fatal("unchanged heavy tool output was deep-copied between generations")
	}
	if fieldString(secondGenerationTool, "startedAt") != "2026-07-24T00:00:00Z" {
		t.Fatalf("tool overlay was not applied: %#v", secondGenerationTool)
	}

	store.mu.Lock()
	tx := store.beginSessionMutationLocked()
	workingMessage := tx.messageForWrite("tab-1", "m1")
	workingTool := findTool(mapFromAnyMain(workingMessage), "tool-1")
	workingOutput := mapFromAnyMain(workingTool["output"])
	if reflect.ValueOf(workingTool).Pointer() == reflect.ValueOf(secondGenerationTool).Pointer() {
		store.abortSessionMutationLocked(tx)
		store.mu.Unlock()
		t.Fatal("mutable snapshot alias shares a tool-event map with the immutable generation")
	}
	if reflect.ValueOf(workingOutput).Pointer() != reflect.ValueOf(secondOutput).Pointer() {
		store.abortSessionMutationLocked(tx)
		store.mu.Unlock()
		t.Fatal("daemon-owned immutable tool payload was reconstructed in the compatibility working root")
	}
	workingTool["startedAt"] = "mutated working copy"
	store.commitSessionMutationLocked(tx)
	store.mu.Unlock()
	if fieldString(secondGenerationTool, "startedAt") == "mutated working copy" {
		t.Fatal("working snapshot event mutation reached the immutable generation")
	}
}

func TestSessionSharedSubtreeWritersRemainCopyOnWrite(t *testing.T) {
	// cloneSessionContainers deliberately shares exactly these heavy subtrees.
	// A future append(anySlice(owner["images"]), ...) or equivalent write-through
	// would race the outside-lock generation marshal. Pin replacement-only
	// writers at the source boundary so that regression cannot land silently.
	sharedKeys := map[string]struct{}{"images": {}, "input": {}, "output": {}}
	for _, name := range []string{"session_store.go", "chat_control.go"} {
		path := filepath.Join(".", name)
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			identifier, ok := call.Fun.(*ast.Ident)
			if !ok || identifier.Name != "append" {
				return true
			}
			var sharedKey string
			ast.Inspect(call.Args[0], func(candidate ast.Node) bool {
				index, ok := candidate.(*ast.IndexExpr)
				if !ok {
					return true
				}
				literal, ok := index.Index.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				var key string
				if err := json.Unmarshal([]byte(literal.Value), &key); err == nil {
					if _, shared := sharedKeys[key]; shared {
						sharedKey = key
						return false
					}
				}
				return true
			})
			if sharedKey != "" {
				t.Errorf("%s contains append through shared %q subtree; replace the complete owning value", name, sharedKey)
			}
			return true
		})
	}
}

func TestSessionImageNameMatchesContentAddress(t *testing.T) {
	dir := t.TempDir()
	data := testImagePNG("zz")
	ref, err := persistSessionImageData(dir, data)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	name, ok := validSessionImageRef(ref)
	if !ok {
		t.Fatalf("invalid ref %q", ref)
	}
	if name != sessionImageName(data) {
		t.Fatalf("memoized name %q disagrees with ref %q", sessionImageName(data), name)
	}
	// A second persist must be a no-op that still reports the same ref, and must
	// not depend on re-reading the file.
	again, err := persistSessionImageData(dir, data)
	if err != nil {
		t.Fatalf("persist again: %v", err)
	}
	if again != ref {
		t.Fatalf("repeat persist changed the ref: %q -> %q", ref, again)
	}
	if sessionImageName(testImagePNG("yy")) == name {
		t.Fatal("distinct payloads produced the same content address")
	}
}
