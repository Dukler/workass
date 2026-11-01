package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every example the daemon can print has to pass the schema it is an example of.
func TestEveryToolsExampleValidatesAgainstItsOwnSchema(t *testing.T) {
	for _, tool := range append(agentMCPTools(), browserMCPTools()...) {
		usage := toolCallUsage(tool, "linux")
		start, end := strings.Index(usage, "{"), strings.LastIndex(usage, "}")
		var example map[string]any
		if start < 0 || json.Unmarshal([]byte(usage[start:end+1]), &example) != nil {
			t.Errorf("%s has no parsable example: %q", tool["name"], usage)
			continue
		}
		if err := validateToolArguments(tool, example); err != nil {
			t.Errorf("%s example fails its schema: %v", tool["name"], err)
		}
		if !strings.Contains(usage, toString(tool["name"])) {
			t.Errorf("%s usage does not name it: %q", tool["name"], usage)
		}
	}
	windows := toolCallUsage(catalogTool(t, "workass_read_chat"), "windows")
	if !strings.Contains(windows, "$env:WORKASS_TOOLS_COMMAND") {
		t.Fatalf("windows usage: %q", windows)
	}
}

func TestEveryListedToolHasItsOwnShortSentence(t *testing.T) {
	shorts := toolShortDescriptions()
	catalog := map[string]bool{}
	for _, tool := range append(agentMCPTools(), browserMCPTools()...) {
		name := toString(tool["name"])
		catalog[name] = true
		if !internalWorkassTools[name] && shorts[name] == "" {
			t.Errorf("%s has no short sentence", name)
		}
	}
	for name := range shorts {
		if !catalog[name] {
			t.Errorf("a short sentence for %s, which is not a tool", name)
		}
	}
	for name := range internalWorkassTools {
		if !catalog[name] {
			t.Errorf("internal tool %s is not a tool", name)
		}
	}
}

// A flag's type is the schema's, and only the flag path ever coerces.
func TestFlagArgumentsTakeTheirTypeFromTheSchema(t *testing.T) {
	coerced := coerceFlagArguments(catalogTool(t, "workass_read_chat"), map[string]any{
		"limit": "10", "include_events": "true", "tab_id": "42", "unknown": "1",
	})
	if number, ok := schemaNumber(coerced["limit"]); !ok || number != 10 {
		t.Fatalf("limit = %#v", coerced["limit"])
	}
	if coerced["include_events"] != true || coerced["tab_id"] != "42" || coerced["unknown"] != "1" {
		t.Fatalf("coerced = %#v", coerced)
	}
	waited := coerceFlagArguments(catalogTool(t, "workass_wait_subagents"), map[string]any{
		"subagent_ids": `["a","b"]`, "timeout_ms": "-1",
	})
	if err := validateToolArguments(catalogTool(t, "workass_wait_subagents"), waited); err != nil {
		t.Fatalf("json flag values: %v %#v", err, waited)
	}
	if ids, ok := waited["subagent_ids"].([]any); !ok || len(ids) != 2 {
		t.Fatalf("subagent_ids = %#v", waited["subagent_ids"])
	}
	// A value that is not of the type is left for the validator to refuse by name.
	wrong := coerceFlagArguments(catalogTool(t, "workass_read_chat"), map[string]any{"limit": "many"})
	if err := validateToolArguments(catalogTool(t, "workass_read_chat"), wrong); err == nil || !strings.Contains(err.Error(), "limit must be an integer") {
		t.Fatalf("wrong type: %v", err)
	}
}
