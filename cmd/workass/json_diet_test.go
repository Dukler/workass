package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"workass/internal/httpserve"
	"workass/internal/wire"
)

// A first screen's settled tool calls arrive as what their folded row shows:
// no input, the head of the output, the rest marked deferred for toolDetail.
// A live row, a subagent header and a call that published a page stay whole.
func TestToolSummariesKeepWhatTheFoldedRowShows(t *testing.T) {
	long := strings.Repeat("y", toolSummaryOutputHead*3)
	rows := []any{
		map[string]any{"id": "done", "status": "done", "events": []any{
			map[string]any{"kind": "tool", "key": "a", "command": "ls", "input": `{"command":"ls"}`, "output": long},
			map[string]any{"kind": "tool", "key": "short", "input": "", "output": "ok"},
			map[string]any{"kind": "tool", "key": "agent", "subagentHeader": true, "input": "{}", "output": long},
			map[string]any{"kind": "tool", "key": "page", "command": "workass tools call workass_render_html --title x", "input": "{}", "output": long},
			map[string]any{"kind": "thinking", "key": "t", "text": "why"},
		}},
		map[string]any{"id": "live", "status": "running", "events": []any{
			map[string]any{"kind": "tool", "key": "b", "input": "{}", "output": long},
		}},
	}
	leanArchiveToolSummaries(rows)
	events := anySlice(mapFromAnyMain(rows[0])["events"])
	summary := mapFromAnyMain(events[0])
	if summary["input"] != "" || summary["inputDeferred"] != true || summary["inputBytes"] != len(`{"command":"ls"}`) {
		t.Fatalf("input not deferred: %#v", summary)
	}
	if summary["output"] != long[:toolSummaryOutputHead] || summary["outputDeferred"] != true || summary["outputBytes"] != len(long) || summary["command"] != "ls" {
		t.Fatalf("output not summarized: %#v", summary)
	}
	if short := mapFromAnyMain(events[1]); short["output"] != "ok" || short["outputDeferred"] != nil || short["inputDeferred"] != nil {
		t.Fatalf("a short call changed: %#v", short)
	}
	for _, index := range []int{2, 3} {
		if kept := mapFromAnyMain(events[index]); kept["output"] != long || kept["input"] != "{}" {
			t.Fatalf("event %d was summarized: %#v", index, kept)
		}
	}
	if live := mapFromAnyMain(anySlice(mapFromAnyMain(rows[1])["events"])[0]); live["output"] != long || live["inputDeferred"] != nil {
		t.Fatal("a live row was summarized")
	}
	// An output already leaned keeps the size of the whole payload.
	leaned := []any{map[string]any{"id": "x", "status": "done", "events": []any{
		map[string]any{"kind": "tool", "key": "k", "output": strings.Repeat("z", 1024), "outputDeferred": true, "outputBytes": 99999},
	}}}
	leanArchiveToolSummaries(leaned)
	if event := mapFromAnyMain(anySlice(mapFromAnyMain(leaned[0])["events"])[0]); event["outputBytes"] != 99999 {
		t.Fatalf("a leaned output lost its size: %#v", event)
	}
}

func TestProjectIconByURLNamesTheIconAndServesItImmutably(t *testing.T) {
	root := t.TempDir()
	writeProjectIconFixture(t, root, "public/favicon.png", tinyProjectPNG)
	hub := wire.NewHub()
	registerDaemonHandlers(hub, root, nil)
	read := func(arg map[string]any) map[string]any {
		t.Helper()
		reply, err := hub.Invoke("project:icon", []any{arg})
		if err != nil {
			t.Fatal(err)
		}
		return roundTripJSON(t, reply)
	}
	whole := read(map[string]any{"chatId": "chat-1", "cwd": root, "ifNoneMatch": ""})
	named := read(map[string]any{"chatId": "chat-1", "cwd": root, "ifNoneMatch": "", "byURL": true})
	if named["byURL"] != true || named["base64"] != nil || named["etag"] != whole["etag"] || named["mimeType"] != "image/png" {
		t.Fatalf("byURL reply = %#v (whole etag %v)", named, whole["etag"])
	}
	if receipt := read(map[string]any{"chatId": "chat-1", "cwd": root, "ifNoneMatch": named["etag"], "byURL": true}); receipt["unchanged"] != true {
		t.Fatalf("a held icon by URL = %#v, want a receipt", receipt)
	}

	etag := named["etag"].(string)
	serve := func(path string, header http.Header) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		for key, values := range header {
			request.Header[key] = values
		}
		recorder := httptest.NewRecorder()
		projectIconHTTP{}.ServeHTTP(recorder, request)
		return recorder
	}
	path := httpserve.ProjectIconPathPrefix + etag + "?cwd=" + url.QueryEscape(root)
	got := serve(path, nil)
	if got.Code != http.StatusOK || got.Body.String() != string(tinyProjectPNG) || got.Header().Get("Content-Type") != "image/png" ||
		!strings.Contains(got.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("icon by URL = %d %q %v", got.Code, got.Header().Get("Content-Type"), got.Header())
	}
	if again := serve(path, http.Header{"If-None-Match": {`"` + etag + `"`}}); again.Code != http.StatusNotModified {
		t.Fatalf("a revalidated icon = %d", again.Code)
	}
	writeProjectIconFixture(t, root, "public/favicon.png", append(append([]byte{}, tinyProjectPNG...), 'x'))
	if stale := serve(path, nil); stale.Code != http.StatusNotFound {
		t.Fatalf("an icon that changed after it was named = %d, want 404", stale.Code)
	}
	if bad := serve(httpserve.ProjectIconPathPrefix+"nothex?cwd="+url.QueryEscape(root), nil); bad.Code != http.StatusNotFound {
		t.Fatalf("a malformed icon URL = %d", bad.Code)
	}

	// An SVG icon keeps its bytes on the socket.
	svgRoot := t.TempDir()
	writeProjectIconFixture(t, svgRoot, "favicon.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	if svg := read(map[string]any{"chatId": "chat-1", "cwd": svgRoot, "ifNoneMatch": "", "byURL": true}); svg["byURL"] != nil || svg["base64"] == nil {
		t.Fatalf("an SVG icon by URL = %#v, want its bytes", svg)
	}
}

// A large raster icon is served at the width the sidebar can use.
func TestProjectIconByURLServesASmallRendition(t *testing.T) {
	root := t.TempDir()
	source := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			source.Set(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 13), B: uint8((x ^ y) * 3), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	writeProjectIconFixture(t, root, "public/favicon.png", encoded.Bytes())
	_, _, etag, err := projectIconReplyRaw(root)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, httpserve.ProjectIconPathPrefix+etag+"?cwd="+url.QueryEscape(root), nil)
	recorder := httptest.NewRecorder()
	projectIconHTTP{}.ServeHTTP(recorder, request)
	config, err := png.DecodeConfig(bytes.NewReader(recorder.Body.Bytes()))
	if recorder.Code != http.StatusOK || err != nil || config.Width != projectIconRenditionWidth || recorder.Body.Len() >= encoded.Len() {
		t.Fatalf("rendition = %d, %v, %dx%d, %d bytes (source %d)", recorder.Code, err, config.Width, config.Height, recorder.Body.Len(), encoded.Len())
	}
}

func TestProviderListByEtag(t *testing.T) {
	list := []map[string]any{{"id": "mock", "name": "Mock <dev>", "enabled": true}}
	raw, _ := json.Marshal(list)
	sum := sha256.Sum256(raw)
	held := hex.EncodeToString(sum[:])
	if reply := providersListReply(list, ""); !reflect.DeepEqual(reply, list) {
		t.Fatalf("a plain providers:list = %#v, want the list", reply)
	}
	if reply, _ := providersListReply(list, held).(map[string]any); reply["unchanged"] != true || reply["etag"] != held {
		t.Fatalf("a held providers:list = %#v, want a receipt", reply)
	}
	if reply := providersListReply(list, strings.Repeat("0", 64)); !reflect.DeepEqual(reply, list) {
		t.Fatal("a stale etag must get the list")
	}
}
