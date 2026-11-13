package main

import (
	"context"
	"encoding/json"
	"testing"

	"workass/internal/wire"
)

// A first screen read in two parts is the whole first screen: the last
// exchange, then the tail's rows before it.
func TestFirstScreenPartsMakeTheWholeFirstScreen(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	workGroupTestChat(t, stateDir, backend, "wg")
	runtime := startLazyTestRuntime(t, stateDir, backend, "tab-none")
	defer runtime.Close(context.Background())
	defer workGroupPackStores.Delete(stateDir)
	hub := wire.NewHub()
	registerArchiveHandlers(hub, nil, runtime)
	type reply struct {
		Etag      string            `json:"etag"`
		Unchanged bool              `json:"unchanged"`
		Messages  []json.RawMessage `json:"messages"`
		Part      string            `json:"part"`
		Complete  *bool             `json:"complete"`
	}
	read := func(extra map[string]any) reply {
		t.Helper()
		options := map[string]any{"tail": 9, "lastExchange": true, "toolSummaries": true, "deferImages": true, "leanTools": true, "leanTitles": true, "workGroups": true, "longRows": true}
		for key, value := range extra {
			options[key] = value
		}
		out, err := hub.Invoke("chat:archive-load", []any{"tab-wg", options})
		if err != nil {
			t.Fatal(err)
		}
		var got reply
		if err := json.Unmarshal(mustJSONBytes(t, out), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// The exchange first, as a renderer opening the chat asks: nothing is
	// kept yet, so it is read from its own rows.
	exchange := read(map[string]any{"ifNoneMatch": "", "exchangeFirst": true})
	whole := read(map[string]any{"ifNoneMatch": ""})
	rest := read(map[string]any{"ifNoneMatch": "", "rest": true})
	if exchange.Part != "exchange" || rest.Part != "rest" || rest.Complete == nil || !*rest.Complete {
		t.Fatalf("parts %q %q %v", exchange.Part, rest.Part, rest.Complete)
	}
	if len(exchange.Messages) == 0 || len(exchange.Messages) >= len(whole.Messages) {
		t.Fatalf("exchange has %d of %d rows", len(exchange.Messages), len(whole.Messages))
	}
	joined := append(append([]json.RawMessage(nil), rest.Messages...), exchange.Messages...)
	if string(mustJSONBytes(t, joined)) != string(mustJSONBytes(t, whole.Messages)) {
		t.Fatal("the parts are not the whole first screen")
	}
	if again := read(map[string]any{"ifNoneMatch": exchange.Etag, "exchangeFirst": true}); !again.Unchanged {
		t.Fatal("an unchanged exchange is not a receipt")
	}
}
