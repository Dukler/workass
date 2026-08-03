package wire

import (
	"strings"
	"sync"
	"testing"
)

// A handler panic used to escape Invoke and kill the daemon process. Under
// launchd KeepAlive that reads to the user as an unexplained restart, and it
// takes every chat's ACP engine with it — the blast radius of one bad payload
// was the whole fleet of live turns.
func TestInvokePanicFailsOneCallerInsteadOfTheProcess(t *testing.T) {
	var logged strings.Builder
	var mu sync.Mutex
	hub := NewHub(Options{Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged.WriteString(format)
	}})
	hub.Register("session:get", func([]any) (any, error) {
		var source []byte
		_ = source[:1] // the shape of the real crash: a reslice past capacity
		return nil, nil
	})
	hub.Register("app:meta", func([]any) (any, error) { return "alive", nil })

	result, err := hub.Invoke("session:get", nil)
	if err == nil {
		t.Fatal("panicking handler returned no error")
	}
	if result != nil {
		t.Fatalf("panicking handler returned a result: %#v", result)
	}
	if !strings.Contains(err.Error(), "session:get") {
		t.Fatalf("panic error does not name its channel: %v", err)
	}

	// The stack has to survive into the log, or containing the panic would only
	// convert a loud crash into a silent one.
	mu.Lock()
	saw := logged.String()
	mu.Unlock()
	if !strings.Contains(saw, "handler panic") {
		t.Fatalf("panic was swallowed without a log line: %q", saw)
	}

	// The hub is still serving after the panic — that is the whole point.
	if alive, err := hub.Invoke("app:meta", nil); err != nil || alive != "alive" {
		t.Fatalf("hub unusable after a contained panic: %v %v", alive, err)
	}

	stats := hub.Stats()
	channels, _ := stats["slowestInvokes"].([]map[string]any)
	var counted bool
	for _, channel := range channels {
		if channel["channel"] == "session:get" {
			counted = true
		}
	}
	if !counted {
		t.Fatalf("panicking invoke was not counted in channel stats: %v", stats["slowestInvokes"])
	}
}
