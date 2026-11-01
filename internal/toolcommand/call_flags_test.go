package toolcommand

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/toolcli"
)

// The same vectors drive scripts/tests/workass-tools-client.test.mjs: this is
// the contract that keeps the Windows client and this one reading a command line
// the same way.
func TestParseCallFlagsVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "tests", "fixtures", "tools-call-flags.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Args []string `json:"args"`
		Want struct {
			Input  string            `json:"input"`
			Pairs  map[string]string `json:"pairs"`
			Inline string            `json:"inline"`
			Help   bool              `json:"help"`
		} `json:"want"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors {
		got, err := parseCallFlags(vector.Args)
		if vector.Error != "" {
			if err == nil || !strings.Contains(err.Error(), vector.Error) {
				t.Errorf("%q: err = %v, want %q", vector.Args, err, vector.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", vector.Args, err)
			continue
		}
		if got.input != vector.Want.Input || got.inline != vector.Want.Inline || got.help != vector.Want.Help || len(got.pairs) != len(vector.Want.Pairs) {
			t.Errorf("%q: got %+v want %+v", vector.Args, got, vector.Want)
		}
		for key, value := range vector.Want.Pairs {
			if got.pairs[key] != value {
				t.Errorf("%q: pair %s = %q, want %q", vector.Args, key, got.pairs[key], value)
			}
		}
	}
}

func TestUsageNeverAdvertisesContextAndMatchesTheWindowsClient(t *testing.T) {
	if strings.Contains(Usage, "--context") {
		t.Fatalf("usage advertises the flag the guide forbids: %q", Usage)
	}
	var diagnostics bytes.Buffer
	if err := Run(context.Background(), []string{"--help"}, strings.NewReader(""), &bytes.Buffer{}, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diagnostics.String(), "--context") || !strings.Contains(diagnostics.String(), "--full") {
		t.Fatalf("--help = %q", diagnostics.String())
	}
	client, err := os.ReadFile(filepath.Join("..", "..", "scripts", "workass-tools.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(Usage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(client), string(encoded)) {
		t.Fatalf("the Windows client's usage differs from %s", encoded)
	}
}

func TestExplicitContextFlagStillWorks(t *testing.T) {
	path := fixtureContextFileResponse(t, http.StatusOK, toolcli.Response{Result: map[string]any{"ok": true}})
	t.Setenv("WORKASS_TOOL_CONTEXT", "")
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"--context", path, "list"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestGuidePrintsTheGuideWhateverFollowsIt(t *testing.T) {
	guide := filepath.Join(t.TempDir(), "guide.md")
	if err := os.WriteFile(guide, []byte("the guide"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKASS_TOOLS_GUIDE", guide)
	var output, diagnostics bytes.Buffer
	if err := Run(context.Background(), []string{"guide", "workass_read_chat"}, strings.NewReader(""), &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if output.String() != "the guide" || !strings.Contains(diagnostics.String(), "tools list") {
		t.Fatalf("output=%q diagnostics=%q", output.String(), diagnostics.String())
	}
}

func TestListFullAsksTheDaemonForEverySchema(t *testing.T) {
	queries := make(chan string, 4)
	path := fixtureContextFileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(toolcli.Response{Result: map[string]any{"tools": []any{}}})
	})
	t.Setenv("WORKASS_TOOL_CONTEXT", path)
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"list"}, ""},
		{[]string{"list", "--full"}, "full=1"},
		{[]string{"list", "-full"}, "full=1"},
		{[]string{"list", "workass_read_chat"}, "name=workass_read_chat"},
	} {
		if err := Run(context.Background(), test.args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if got := <-queries; got != test.want {
			t.Fatalf("%v: query %q, want %q", test.args, got, test.want)
		}
	}
}

func TestCallFlagsTravelAsStringsForTheDaemonToType(t *testing.T) {
	type seen struct {
		query string
		body  string
	}
	requests := make(chan seen, 2)
	path := fixtureContextFileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- seen{r.URL.RawQuery, string(body)}
		_ = json.NewEncoder(w).Encode(toolcli.Response{Result: map[string]any{"ok": true}})
	})
	t.Setenv("WORKASS_TOOL_CONTEXT", path)
	if err := Run(context.Background(), []string{"call", "tool", "--limit", "10", "--fork"}, strings.NewReader("ignored"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got := <-requests
	if got.query != "flags=1" || !strings.Contains(got.body, `"limit":"10"`) || !strings.Contains(got.body, `"fork":"true"`) {
		t.Fatalf("flag request = %+v", got)
	}
	if err := Run(context.Background(), []string{"call", "tool"}, strings.NewReader(`{"limit":10}`), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got = <-requests
	if got.query != "" || !strings.Contains(got.body, `"limit":10`) {
		t.Fatalf("JSON request = %+v", got)
	}
}

func TestAFlagBeforeTheToolNameIsNamedAsThatMistake(t *testing.T) {
	t.Setenv("WORKASS_TOOL_CONTEXT", "")
	err := Run(context.Background(), []string{"call", "--title", "x", "some_tool"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "tool name first") {
		t.Fatalf("err = %v", err)
	}
}

func TestACallSyntaxMistakeAsksForThatToolsExample(t *testing.T) {
	queries := make(chan string, 2)
	path := fixtureContextFileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		queries <- r.Method + " " + r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(toolcli.Response{Result: map[string]any{"usage": "the example for this tool"}})
	})
	t.Setenv("WORKASS_TOOL_CONTEXT", path)
	err := Run(context.Background(), []string{"call", "some_tool", "stray"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unexpected") || !strings.Contains(err.Error(), "the example for this tool") {
		t.Fatalf("err = %v", err)
	}
	if got := <-queries; got != "GET name=some_tool&usage=1" {
		t.Fatalf("request = %q", got)
	}
	// With no context to ask through, the mistake is still explained.
	t.Setenv("WORKASS_TOOL_CONTEXT", "")
	err = Run(context.Background(), []string{"call", "some_tool", "stray"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unexpected") || !strings.Contains(err.Error(), "--input") {
		t.Fatalf("err = %v", err)
	}
}
