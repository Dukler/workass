package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestBrowserMutationReleasesActorLockAndSerializesConcurrentRetry(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	actor, err := harness.runtime.actor("mcp-chat")
	if err != nil {
		t.Fatal(err)
	}

	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	operationID := providercontract.OperationID("browser-concurrent-retry")
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	firstDone := make(chan error, 1)
	go func() {
		_, callErr := harness.runtime.executeBrowserMutation(
			context.Background(), "mcp-tab", "mcp-chat", operationID,
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

	actorLockAvailable := make(chan struct{})
	go func() {
		actor.mu.Lock()
		actor.mu.Unlock()
		close(actorLockAvailable)
	}()
	select {
	case <-actorLockAvailable:
	case <-time.After(time.Second):
		t.Fatal("actor state lock remained held during external browser dispatch")
	}

	var duplicateDispatches atomic.Int32
	var prematureReadbacks atomic.Int32
	retryDone := make(chan error, 1)
	go func() {
		_, callErr := harness.runtime.executeBrowserMutation(
			context.Background(), "mcp-tab", "mcp-chat", operationID,
			"workass_browser_click", "browser.click", digest,
			func() (browserControlReply, error) {
				duplicateDispatches.Add(1)
				return browserControlReply{}, nil
			},
			func() (browserControlReply, error) {
				prematureReadbacks.Add(1)
				return browserControlReply{}, nil
			},
		)
		retryDone <- callErr
	}()

	select {
	case err := <-retryDone:
		t.Fatalf("concurrent retry escaped serialization before dispatch receipt: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if duplicateDispatches.Load() != 0 || prematureReadbacks.Load() != 0 {
		t.Fatalf("retry raced browser dispatch: dispatches=%d readbacks=%d", duplicateDispatches.Load(), prematureReadbacks.Load())
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first mutation: %v", err)
	}
	if err := <-retryDone; err != nil {
		t.Fatalf("serialized retry: %v", err)
	}
	if duplicateDispatches.Load() != 0 || prematureReadbacks.Load() != 0 {
		t.Fatalf("terminal retry performed external work: dispatches=%d readbacks=%d", duplicateDispatches.Load(), prematureReadbacks.Load())
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
