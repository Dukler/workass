package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

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
	handler, ok := newBrowserStatelessMCPHandler(harness.manager, controlFile, harness.runtime).(*statelessMCPHandler)
	if !ok {
		t.Fatal("browser stateless MCP handler has unexpected concrete type")
	}
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
