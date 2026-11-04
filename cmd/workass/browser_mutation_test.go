package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// Calls to different tabs of one chat reach the shell together, where each tab
// has its own queue. The proof is an order, not a time: the shell answers the
// held call only after the other call has reached it, which cannot happen
// while the held call keeps the other waiting in the daemon. The minute only
// ends a run where it does.
func TestBrowserCallsToDifferentTabsOfOneChatRunTogether(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	controlFile := filepath.Join(t.TempDir(), "browser-control.json")
	if err := os.WriteFile(controlFile, []byte(browserTestDescriptor), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	heldArrived := map[string]chan struct{}{}
	otherArrived := map[string]chan struct{}{}
	sawOther := map[string]chan bool{}
	signal := func(channels map[string]chan struct{}, key string) {
		mu.Lock()
		defer mu.Unlock()
		select {
		case <-channels[key]:
		default:
			close(channels[key])
		}
	}
	handler := newWorkassToolHandler(harness.manager, nil, controlFile, harness.runtime)
	handler.browserClient = &http.Client{Transport: browserRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, err
		}
		reply := map[string]any{"id": payload["id"], "operationId": payload["operationId"], "requestDigest": payload["requestDigest"], "receipt": true}
		if payload["method"] == "browser.controlStatus" {
			reply = map[string]any{"id": payload["id"], "result": map[string]any{"ready": true, "controller": true, "instanceId": browserTestInstanceID}}
		} else {
			operationID := browserString(payload["operationId"])
			pair := strings.TrimPrefix(strings.TrimPrefix(operationID, "held-"), "other-")
			if strings.HasPrefix(operationID, "held-") {
				signal(heldArrived, pair)
				mu.Lock()
				other, saw := otherArrived[pair], sawOther[pair]
				mu.Unlock()
				select {
				case <-other:
					saw <- true
				case <-time.After(time.Minute):
					saw <- false
				}
			} else {
				signal(otherArrived, pair)
			}
			reply["result"] = map[string]any{"ok": true}
		}
		body, _ := json.Marshal(reply)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	call := func(name string, arguments map[string]any) map[string]any {
		body, _ := json.Marshal(map[string]any{"name": name, "arguments": arguments})
		request, err := http.NewRequest(http.MethodPost, server.URL+toolsPath, bytes.NewReader(body))
		if err != nil {
			t.Error(err)
			return nil
		}
		request.Header.Set("Authorization", "Bearer mcp-owner")
		request.Header.Set("X-Workass-Chat-ID", "mcp-chat")
		request.Header.Set("X-Workass-Tab-ID", "mcp-tab")
		request.Header.Set("Content-Type", "application/json")
		reply, err := server.Client().Do(request)
		if err != nil {
			t.Error(err)
			return nil
		}
		defer reply.Body.Close()
		var response map[string]any
		if err := json.NewDecoder(reply.Body).Decode(&response); err != nil {
			t.Error(err)
		}
		return response
	}
	failed := func(response map[string]any) bool {
		encoded, _ := json.Marshal(response)
		return response == nil || strings.TrimSpace(browserString(response["error"])) != "" || strings.Contains(string(encoded), `"isError":true`)
	}
	type browserCall struct {
		name      string
		arguments map[string]any
	}
	click := func(tab int) browserCall {
		arguments := map[string]any{"selector": "#next"}
		if tab > 0 {
			arguments["tab_id"] = tab
		}
		return browserCall{"workass_browser_click", arguments}
	}
	newTab := browserCall{"workass_browser_open", map[string]any{"new_tab": true, "url": "http://127.0.0.1:9/page"}}
	for _, pair := range []struct {
		name        string
		held, other browserCall
	}{
		{"two tabs", click(7), click(8)},
		{"a tab and the own page", click(7), click(0)},
		{"the own page and a tab", click(0), click(8)},
		{"a tab and a new tab", click(7), newTab},
		{"two new tabs", newTab, newTab},
	} {
		t.Run(pair.name, func(t *testing.T) {
			key := strings.ReplaceAll(pair.name, " ", "-")
			arrived, saw := make(chan struct{}), make(chan bool, 1)
			mu.Lock()
			heldArrived[key], otherArrived[key], sawOther[key] = arrived, make(chan struct{}), saw
			mu.Unlock()
			arguments := func(base map[string]any, operationID string) map[string]any {
				out := map[string]any{"operation_id": operationID}
				for name, value := range base {
					out[name] = value
				}
				return out
			}
			heldDone := make(chan map[string]any, 1)
			go func() { heldDone <- call(pair.held.name, arguments(pair.held.arguments, "held-"+key)) }()
			select {
			case <-arrived:
			case response := <-heldDone:
				t.Fatalf("held call ended before it reached the shell: %#v", response)
			}
			if response := call(pair.other.name, arguments(pair.other.arguments, "other-"+key)); failed(response) {
				t.Fatalf("other call: %#v", response)
			}
			if !<-saw {
				t.Fatal("the other call reached the shell only after the held call ended")
			}
			if response := <-heldDone; failed(response) {
				t.Fatalf("held call: %#v", response)
			}
		})
	}
}

func TestBrowserMutationReleasesActorLockAndSerializesConcurrentRetry(t *testing.T) {
	engine, err := chat.NewEngine("browser-mutation-chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(chat.InitializeChat{
		Presentation: chat.PresentationState{TabID: "browser-mutation-tab"},
		OperationID:  "browser-mutation-create", Digest: "browser-mutation-create",
	}); err != nil {
		t.Fatal(err)
	}
	actor := &providerChatActor{engine: engine}
	runtime := &providerChatRuntime{
		manager: &acp.Manager{}, sessions: &sessionStore{},
		actors: map[string]*providerChatActor{"browser-mutation-chat": actor},
		known:  map[string]struct{}{"browser-mutation-chat": {}},
	}

	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	operationID := providercontract.OperationID("browser-concurrent-retry")
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, callErr := runtime.executeBrowserMutation(
			context.Background(), "browser-mutation-tab", "browser-mutation-chat", operationID,
			"workass_browser_click", "browser.click", digest,
			func() (browserControlReply, error) {
				close(started)
				<-release
				return browserControlReply{
					OperationID: string(operationID), RequestDigest: digest, Receipt: true,
					Result: map[string]any{"clicked": true},
				}, nil
			},
			nil,
		)
		firstDone <- callErr
	}()
	<-started

	if !actor.mu.TryLock() {
		t.Fatal("actor state lock remained held during external browser dispatch")
	}
	actor.mu.Unlock()
	if actor.externalMutationMu.TryLock() {
		actor.externalMutationMu.Unlock()
		t.Fatal("external browser dispatch did not hold the exact retry serialization boundary")
	}

	close(release)
	if firstErr := <-firstDone; firstErr != nil {
		t.Fatalf("first mutation: %v", firstErr)
	}
	duplicateDispatches, prematureReadbacks := 0, 0
	if _, retryErr := runtime.executeBrowserMutation(
		context.Background(), "browser-mutation-tab", "browser-mutation-chat", operationID,
		"workass_browser_click", "browser.click", digest,
		func() (browserControlReply, error) {
			duplicateDispatches++
			return browserControlReply{}, nil
		},
		func() (browserControlReply, error) {
			prematureReadbacks++
			return browserControlReply{}, nil
		},
	); retryErr != nil {
		t.Fatalf("serialized retry: %v", retryErr)
	}
	if duplicateDispatches != 0 || prematureReadbacks != 0 {
		t.Fatalf("terminal retry performed external work: dispatches=%d readbacks=%d", duplicateDispatches, prematureReadbacks)
	}
}

func TestBrowserMutationLaneIsTheTabTheCallDrives(t *testing.T) {
	for _, item := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"selector": "#a"}, ""},
		{map[string]any{"tabId": json.Number("7")}, "tab:7"},
		{map[string]any{"tabId": float64(7)}, "tab:7"},
		{map[string]any{"tabId": json.Number("7.0")}, "tab:7"},
		{map[string]any{"newTab": true, "url": "http://127.0.0.1:9/"}, "new-tab:op-1"},
		{map[string]any{"newTab": false}, ""},
	} {
		if got := browserMutationLane(item.params, "op-1"); got != item.want {
			t.Errorf("lane for %#v = %q, want %q", item.params, got, item.want)
		}
	}
}

// A call to one tab holds that tab's lane, not the chat's lock, and a retry of
// it on the same lane still finds the first dispatch's terminal receipt.
func TestBrowserTabCallHoldsItsLaneAndItsRetryWaitsThere(t *testing.T) {
	engine, err := chat.NewEngine("browser-lane-chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(chat.InitializeChat{
		Presentation: chat.PresentationState{TabID: "browser-lane-tab"},
		OperationID:  "browser-lane-create", Digest: "browser-lane-create",
	}); err != nil {
		t.Fatal(err)
	}
	actor := &providerChatActor{engine: engine}
	runtime := &providerChatRuntime{
		manager: &acp.Manager{}, sessions: &sessionStore{},
		actors: map[string]*providerChatActor{"browser-lane-chat": actor},
		known:  map[string]struct{}{"browser-lane-chat": {}},
	}
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	operationID := providercontract.OperationID("browser-lane-call")
	execute := func(dispatch, readback func() (browserControlReply, error)) error {
		_, err := runtime.executeBrowserMutationWithAdmission(context.Background(), "browser-lane-tab", "browser-lane-chat", operationID,
			"workass_browser_click", "browser.click", digest, "tab:7", nil, dispatch, readback)
		return err
	}
	started, release, firstDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		firstDone <- execute(func() (browserControlReply, error) {
			close(started)
			<-release
			return browserControlReply{OperationID: string(operationID), RequestDigest: digest, Receipt: true, Result: map[string]any{"clicked": true}}, nil
		}, nil)
	}()
	<-started
	actor.browserLanes.mu.Lock()
	lane := actor.browserLanes.lanes["tab:7"]
	actor.browserLanes.mu.Unlock()
	if lane == nil || lane.mu.TryLock() {
		t.Fatal("the call did not hold its tab's lane while it reached the shell")
	}
	if !actor.externalMutationMu.TryLock() {
		t.Fatal("a call to a tab held the chat's lock")
	}
	actor.externalMutationMu.Unlock()
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first call: %v", err)
	}
	actor.browserLanes.mu.Lock()
	left := len(actor.browserLanes.lanes)
	actor.browserLanes.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d lanes kept after their calls ended", left)
	}
	dispatches, readbacks := 0, 0
	if err := execute(
		func() (browserControlReply, error) { dispatches++; return browserControlReply{}, nil },
		func() (browserControlReply, error) { readbacks++; return browserControlReply{}, nil },
	); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if dispatches != 0 || readbacks != 0 {
		t.Fatalf("retry reached the shell: dispatches=%d readbacks=%d", dispatches, readbacks)
	}
}

func TestBrowserReadReleasesActorLockDuringShellHTTP(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	actor, err := harness.runtime.actor("mcp-chat")
	if err != nil {
		t.Fatal(err)
	}
	controlFile := filepath.Join(t.TempDir(), "browser-control.json")
	if err := os.WriteFile(controlFile, []byte(browserTestDescriptor), 0o600); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	handler := newWorkassToolHandler(harness.manager, nil, controlFile, harness.runtime)
	handler.kind = browserToolKind
	handler.browserClient = &http.Client{Transport: browserRoundTripFunc(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":1,"result":{"tabs":[]}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	readDone := make(chan error, 1)
	go func() {
		_, callErr := handler.callTool(nil, browserMCPCallParams{
			Name: "workass_browser_list", Arguments: map[string]any{},
		}, "", "mcp-chat", "mcp-tab")
		readDone <- callErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("browser read did not reach shell HTTP")
	}

	actorLockAvailable := make(chan struct{})
	go func() {
		actor.mu.Lock()
		actor.mu.Unlock()
		close(actorLockAvailable)
	}()
	select {
	case <-actorLockAvailable:
	case <-time.After(time.Second):
		t.Fatal("actor state lock remained held during browser read HTTP")
	}

	close(release)
	if err := <-readDone; err != nil {
		t.Fatalf("browser read: %v", err)
	}
}
