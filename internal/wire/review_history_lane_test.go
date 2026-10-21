//go:build review

// Independent-review reproduction (b8b18322..fe7003e5). Run with:
//   go test -tags review -count=1 -run TestReview ./internal/wire/

package wire

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// Review: a disconnect while a history read is still running must not strand
// the connection's lanes or its registration once the read returns.
func TestReviewHistoryLaneDrainsOnDisconnect(t *testing.T) {
	hub := NewHub()
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	hub.RegisterHistoryRead("chat:archive-load", func(args []any) (any, error) {
		started <- struct{}{}
		<-release
		return []any{}, nil
	})
	hub.Register("providers:detect", func(args []any) (any, error) {
		started <- struct{}{}
		<-release
		return map[string]any{}, nil
	})
	server := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	defer server.Close()
	baseline := runtime.NumGoroutine()
	for round := 0; round < 5; round++ {
		client := dialWS(t, server.URL, "cmV2aWV3LWhpc3RvcnktbGFuZQ==")
		client.sendText(t, `{"t":"invoke","id":1,"channel":"chat:archive-load","args":["a"]}`)
		client.sendText(t, `{"t":"invoke","id":2,"channel":"providers:detect","args":[]}`)
		for i := 0; i < 2; i++ {
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("handler never started")
			}
		}
		_ = client.conn.Close()
		time.Sleep(50 * time.Millisecond)
		release <- struct{}{}
		release <- struct{}{}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		hub.mu.Lock()
		clients := len(hub.clients)
		hub.mu.Unlock()
		if clients == 0 && runtime.NumGoroutine() <= baseline+2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after disconnects: clients=%d goroutines=%d baseline=%d", clients, runtime.NumGoroutine(), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
