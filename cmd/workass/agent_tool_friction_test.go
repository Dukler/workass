package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/fleetclient"
)

// The measurements behind these tests: a 79.5 KB chat list, a 98.9 KB tool
// catalog, and wait and subsession calls failing about half the time.

func frictionCall(t *testing.T, h statelessMCPTestHarness, name string, arguments map[string]any) (map[string]any, string) {
	t.Helper()
	status, response := h.request(t, http.MethodPost, map[string]any{"name": name, "arguments": arguments})
	if failure, ok := response["error"].(string); ok && failure != "" {
		return nil, failure
	}
	if status != http.StatusOK {
		t.Fatalf("%s status=%d response=%#v", name, status, response)
	}
	return mapFromAnyMain(response["result"]), ""
}

func frictionMustCall(t *testing.T, h statelessMCPTestHarness, name string, arguments map[string]any) map[string]any {
	t.Helper()
	result, failure := frictionCall(t, h, name, arguments)
	if failure != "" {
		t.Fatalf("%s %#v: %s", name, arguments, failure)
	}
	return result
}

func frictionRow(rows []any, chatID string) map[string]any {
	for _, raw := range rows {
		if row := mapFromAnyMain(raw); fieldString(row, "chatId") == chatID {
			return row
		}
	}
	return nil
}

// settledFleetFixture is rows shaped as a mesh peer's chat.list arrives here,
// tagged for their home by homedChatRows.
func settledFleetFixture(total, settled int) []any {
	rows := make([]any, 0, total)
	for i := 0; i < total; i++ {
		row := map[string]any{
			"tabId": fmt.Sprintf("M~m-peer~tab-%d", i), "chatId": fmt.Sprintf("M~m-peer~chat-%d", i),
			"title": fmt.Sprintf("Investigate the failing nightly build number %d", i),
			"cwd":   "/srv/workspace/some/project/checkout", "providerId": "claude",
			"modelId": "claude-opus-5-5", "modeId": "bypassPermissions", "active": false,
			"settled": "", "settledAt": 0, "running": false, "machineId": "m-peer",
		}
		if i < settled {
			row["settled"], row["settledAt"] = "settled", 1792187130561+i
		}
		rows = append(rows, row)
	}
	return rows
}

func TestToolFrictionListChatsLeavesSettledOutAndSaysHowToGetThem(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	h.handler.agentControl.remoteChats = &stubAgentChatRemoteRouter{result: map[string]any{"chats": settledFleetFixture(261, 255)}}

	request, _ := http.NewRequest(http.MethodPost, h.server.URL+toolsPath, nil)
	raw, err := h.handler.agentControl.call(request, agentControlRequest{Method: "chat.list", Params: map[string]any{
		"parent_chat_id": "mcp-chat", "parent_tab_id": "mcp-tab", "owner_key": "mcp-owner",
	}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(raw)

	listed := frictionMustCall(t, h, "workass_list_chats", map[string]any{})
	after, _ := json.Marshal(listed)
	t.Logf("workass_list_chats on 262 chats, 255 settled: %d bytes before, %d after", len(before), len(after))
	if len(after)*10 > len(before) {
		t.Fatalf("the default list is still %d of %d bytes", len(after), len(before))
	}
	rows := anySlice(listed["chats"])
	if len(rows) != 7 {
		t.Fatalf("default list has %d rows, want 6 live peers and this chat: %s", len(rows), after)
	}
	if count, _ := schemaNumber(listed["settledOmitted"]); count != 255 {
		t.Fatalf("settled left out = %v: %s", listed["settledOmitted"], after)
	}
	if note := fieldString(listed, "note"); !strings.Contains(note, "255") || !strings.Contains(note, "include_settled") {
		t.Fatalf("the list does not say how to get the rest: %q", note)
	}
	live := frictionRow(rows, "M~m-peer~chat-260")
	if live == nil || fieldString(live, "machineId") != "m-peer" || fieldString(live, "tabId") != "M~m-peer~tab-260" {
		t.Fatalf("machine-tagged row lost its address: %#v", live)
	}
	for _, dropped := range []string{"settled", "settledAt", "running", "active"} {
		if _, present := live[dropped]; present {
			t.Fatalf("empty field %s stayed on the row: %#v", dropped, live)
		}
	}

	// Everything is still reachable, by the same tagged ids.
	all := frictionMustCall(t, h, "workass_list_chats", map[string]any{"include_settled": true})
	allRows := anySlice(all["chats"])
	if len(allRows) != 262 || all["settledOmitted"] != nil {
		t.Fatalf("include_settled listed %d rows: settledOmitted=%v", len(allRows), all["settledOmitted"])
	}
	settled := frictionRow(allRows, "M~m-peer~chat-3")
	if fieldString(settled, "settled") != "settled" {
		t.Fatalf("a settled row does not say so: %#v", settled)
	}

	found := frictionMustCall(t, h, "workass_list_chats", map[string]any{"query": "NIGHTLY BUILD NUMBER 3", "include_settled": true})
	if got := len(anySlice(found["chats"])); got != 11 {
		t.Fatalf("query matched %d rows, want number 3 and 30-39", got)
	}
	byCWD := frictionMustCall(t, h, "workass_list_chats", map[string]any{"query": "some/project"})
	if got := len(anySlice(byCWD["chats"])); got != 6 {
		t.Fatalf("cwd query matched %d live rows", got)
	}
	if count, _ := schemaNumber(byCWD["settledOmitted"]); count != 255 {
		t.Fatalf("a query must count the settled chats it would have matched: %v", byCWD["settledOmitted"])
	}
	none := frictionMustCall(t, h, "workass_list_chats", map[string]any{"query": "matches nothing at all"})
	if len(anySlice(none["chats"])) != 0 || none["settledOmitted"] != nil {
		t.Fatalf("an empty search reported settled chats it never matched: %#v", none)
	}
	limited := frictionMustCall(t, h, "workass_list_chats", map[string]any{"limit": 2})
	if got := len(anySlice(limited["chats"])); got != 2 {
		t.Fatalf("limit returned %d rows", got)
	}
	if count, _ := schemaNumber(limited["omittedByLimit"]); count != 5 {
		t.Fatalf("limit did not say what it cut: %#v", limited["omittedByLimit"])
	}
	if _, failure := frictionCall(t, h, "workass_list_chats", map[string]any{"limit": 0}); !strings.Contains(failure, "limit") {
		t.Fatalf("limit 0 = %q", failure)
	}

	// The filter lives on the tool. The wire method peers and the renderer read
	// answers every row, exactly as before.
	if router := h.handler.agentControl.remoteChats.(*stubAgentChatRemoteRouter); len(router.params) != 0 {
		t.Fatalf("the list options crossed the route: %#v", router.params)
	}
	wire, err := h.handler.agentControl.callAsController(context.Background(), map[string]any{"method": "chat.list", "params": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	row := frictionRow(anySlice(mapFromAnyMain(wire)["chats"]), "mcp-chat")
	if _, kept := row["running"]; !kept {
		t.Fatalf("the wire row changed shape: %#v", row)
	}
	if _, kept := row["self"]; kept {
		t.Fatalf("a wire row claims a caller: %#v", row)
	}
}

func TestToolFrictionListedRemoteIDsStillRoute(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	router := &stubAgentChatRemoteRouter{result: map[string]any{"chats": settledFleetFixture(2, 1)}}
	h.handler.agentControl.remoteChats = router
	rows := anySlice(frictionMustCall(t, h, "workass_list_chats", map[string]any{"include_settled": true})["chats"])
	row := frictionRow(rows, "M~m-peer~chat-0")
	if row == nil {
		t.Fatalf("no remote row: %#v", rows)
	}
	router.result = map[string]any{"chatId": "chat-0", "messages": []any{}}
	frictionMustCall(t, h, "workass_read_chat", map[string]any{"tab_id": fieldString(row, "tabId"), "chat_id": fieldString(row, "chatId")})
	if router.method != "chat.read" || fieldString(router.params, "machine_id") != "m-peer" {
		t.Fatalf("a listed tagged id did not route to its machine: %s %#v", router.method, router.params)
	}
}

// The whole path with two real daemons: the home answers chat.list with every
// row, this daemon's tool filters what it merged, and a tagged id out of that
// answer reaches the home again. With a wait that names no operation id on top,
// which a home has to accept from a caller that never heard of it.
func TestToolFrictionFleetListFiltersMergedRowsAndTheirIDsStillRoute(t *testing.T) {
	secret := newFleetTestSecret(t)
	home := newStatelessMCPTestHarness(t)
	frictionMustCall(t, home, "workass_create_chat", map[string]any{"operation_id": "home-live", "title": "still live on the home"})
	filed := frictionMustCall(t, home, "workass_create_chat", map[string]any{"operation_id": "home-filed", "title": "filed on the home"})
	frictionMustCall(t, home, "workass_settle_chat", map[string]any{
		"operation_id": "home-settle", "tab_id": fieldString(filed, "tabId"), "chat_id": fieldString(filed, "chatId"), "settled": true,
	})
	served := serveMeshHome(t, "m-home", secret, home.handler.agentControl)

	away := newStatelessMCPTestHarness(t)
	mesh := newMeshDialer(t, "m-away", secret)
	if err := mesh.AddRoute(served.machineID, "home", fleetclient.KindLAN, served.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	relay := &refusingRelay{t: t, allow: true, err: errNoLocalRenderer}
	away.handler.agentControl.remoteChats = newFleetMeshRouter(mesh, relay, "m-away", t.Logf)

	listed := frictionMustCall(t, away, "workass_list_chats", map[string]any{})
	rows := anySlice(listed["chats"])
	titles := map[string]map[string]any{}
	for _, raw := range rows {
		row := mapFromAnyMain(raw)
		titles[fieldString(row, "title")] = row
	}
	row := titles["still live on the home"]
	if row == nil || fieldString(row, "machineId") != "m-home" || !strings.HasPrefix(fieldString(row, "chatId"), "M~m-home~") {
		t.Fatalf("the home's live chat is missing or unaddressed: %#v", rows)
	}
	if titles["filed on the home"] != nil {
		t.Fatalf("the home's settled chat stayed in the default list: %#v", rows)
	}
	if count, _ := schemaNumber(listed["settledOmitted"]); count != 1 {
		t.Fatalf("settledOmitted = %v: %#v", listed["settledOmitted"], listed)
	}
	if fieldString(mapFromAnyMain(listed["self"]), "chat_id") != "mcp-chat" {
		t.Fatalf("self = %#v", listed["self"])
	}
	if frictionRow(rows, "M~m-home~mcp-chat") == nil || frictionRow(rows, "M~m-home~mcp-chat")["self"] != nil {
		t.Fatalf("the home's chat of the same name was marked as this caller: %#v", frictionRow(rows, "M~m-home~mcp-chat"))
	}
	read := frictionMustCall(t, away, "workass_read_chat", map[string]any{"tab_id": fieldString(row, "tabId"), "chat_id": fieldString(row, "chatId")})
	if fieldString(read, "title") != "still live on the home" {
		t.Fatalf("a tagged id out of the filtered list did not reach its chat: %#v", read)
	}
	all := frictionMustCall(t, away, "workass_list_chats", map[string]any{"include_settled": true, "query": "filed on the home"})
	found := anySlice(all["chats"])
	if len(found) != 1 || fieldString(mapFromAnyMain(found[0]), "settled") != "settled" {
		t.Fatalf("include_settled over the mesh: %#v", all)
	}
	// A remote wait with nothing named is the home's to resolve, and its answer is
	// the shape a local one has.
	remoteTab, remoteChat := fieldString(mapFromAnyMain(titles["still live on the home"]), "tabId"), fieldString(titles["still live on the home"], "chatId")
	if _, failure := frictionCall(t, away, "workass_wait_subagents", map[string]any{"tab_id": remoteTab, "chat_id": remoteChat, "timeout_ms": 1000}); !strings.Contains(failure, "no subagents") {
		t.Fatalf("a remote wait with no ids: %q", failure)
	}
}

// A delegated agent reads the catalog it may call, in the same short form.
func TestToolFrictionChildListingIsShortToo(t *testing.T) {
	rows := anySlice(toolListing(agentToolsForChild())["tools"])
	if len(rows) == 0 {
		t.Fatal("empty child listing")
	}
	applyListed := false
	for _, raw := range rows {
		name := fieldString(mapFromAnyMain(raw), "name")
		if name == "workass_register_process_child" || strings.Contains(name, "subagent") || strings.Contains(name, "fork") {
			t.Fatalf("%s is not for a child's listing", name)
		}
		applyListed = applyListed || name == "workass_apply_update"
	}
	if !applyListed {
		t.Fatal("a delegated agent lost workass_apply_update")
	}
}

func TestToolFrictionListMarksTheCallingChatEvenWhenFilteredOut(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	listed := frictionMustCall(t, h, "workass_list_chats", map[string]any{})
	self := mapFromAnyMain(listed["self"])
	if fieldString(self, "tab_id") != "mcp-tab" || fieldString(self, "chat_id") != "mcp-chat" {
		t.Fatalf("self = %#v", listed["self"])
	}
	if row := frictionRow(anySlice(listed["chats"]), "mcp-chat"); row == nil || row["self"] != true {
		t.Fatalf("the calling row is not marked: %#v", listed["chats"])
	}
	filtered := frictionMustCall(t, h, "workass_list_chats", map[string]any{"query": "matches nothing at all"})
	if len(anySlice(filtered["chats"])) != 0 || fieldString(mapFromAnyMain(filtered["self"]), "chat_id") != "mcp-chat" {
		t.Fatalf("self went missing with its row: %#v", filtered)
	}

	created := frictionMustCall(t, h, "workass_create_chat", map[string]any{"operation_id": "friction-create", "title": "helper chat"})
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	frictionMustCall(t, h, "workass_settle_chat", map[string]any{"operation_id": "friction-settle", "tab_id": tabID, "chat_id": chatID, "settled": true})
	listed = frictionMustCall(t, h, "workass_list_chats", map[string]any{})
	if frictionRow(anySlice(listed["chats"]), chatID) != nil {
		t.Fatalf("a settled local chat stayed in the default list: %#v", listed)
	}
	if count, _ := schemaNumber(listed["settledOmitted"]); count != 1 {
		t.Fatalf("settledOmitted = %v", listed["settledOmitted"])
	}
	if frictionRow(anySlice(frictionMustCall(t, h, "workass_list_chats", map[string]any{"include_settled": true})["chats"]), chatID) == nil {
		t.Fatal("include_settled lost a settled local chat")
	}
}

func TestToolFrictionSingleChatToolsDefaultToTheCallingChat(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	h.handler.agentControl.forks = newForkService(h.manager.StateDir(), h.manager, nil)
	for _, name := range []string{
		"workass_read_chat", "workass_get_chat_diagnostics", "workass_list_spawned_work",
		"workass_list_spawned_work_receipts", "workass_list_forks",
	} {
		frictionMustCall(t, h, name, map[string]any{})
		frictionMustCall(t, h, name, map[string]any{"tab_id": "mcp-tab", "chat_id": "mcp-chat"})
		for _, half := range []map[string]any{{"tab_id": "mcp-tab"}, {"chat_id": "mcp-chat"}} {
			if _, failure := frictionCall(t, h, name, half); !strings.Contains(failure, "is required") {
				t.Fatalf("%s accepted %#v: %q", name, half, failure)
			}
		}
	}
	if read := frictionMustCall(t, h, "workass_read_chat", map[string]any{}); fieldString(read, "chatId") != "mcp-chat" {
		t.Fatalf("read did not default to the caller: %#v", read)
	}
	if _, failure := frictionCall(t, h, "workass_create_fork_point", map[string]any{"operation_id": "friction-fork", "name": "fp"}); strings.Contains(failure, "tab_id") || strings.Contains(failure, "chat_id") {
		t.Fatalf("fork point refused the calling chat: %q", failure)
	}
	if _, failure := frictionCall(t, h, "workass_create_fork_point", map[string]any{"operation_id": "friction-fork-half", "name": "fp", "tab_id": "mcp-tab"}); !strings.Contains(failure, "is required") {
		t.Fatalf("fork point accepted half a pair: %q", failure)
	}
	frictionMustCall(t, h, "workass_rename_chat", map[string]any{"operation_id": "friction-rename", "title": "renamed by default"})
	if state, _ := h.runtime.Snapshot("mcp-chat"); state.Presentation.Title != "renamed by default" {
		t.Fatalf("rename did not act on the caller: %q", state.Presentation.Title)
	}
	// A destructive or outward act never falls back on the caller.
	for name, arguments := range map[string]map[string]any{
		"workass_delete_chat":       {"operation_id": "friction-delete"},
		"workass_cancel_chat_turn":  {"operation_id": "friction-cancel"},
		"workass_send_chat_message": {"operation_id": "friction-send", "message": "hello"},
		"workass_settle_chat":       {"operation_id": "friction-settle-self", "settled": true},
		"workass_focus_chat":        {"operation_id": "friction-focus"},
	} {
		if _, failure := frictionCall(t, h, name, arguments); !strings.Contains(failure, "tab_id is required") && !strings.Contains(failure, "chat_id is required") {
			t.Fatalf("%s defaulted to the caller: %q", name, failure)
		}
	}
}

func frictionCLI(t *testing.T, contextFile string, input string, args ...string) (string, error) {
	t.Helper()
	var output, diagnostics bytes.Buffer
	err := runToolsCommand(context.Background(), append([]string{"--context", contextFile}, args...), strings.NewReader(input), &output, &diagnostics)
	return output.String(), err
}

func TestToolFrictionListingIsShortAndFullOnRequest(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	contextFile, _ := toolCLIContextFixture(t, h.handler)
	full, err := frictionCLI(t, contextFile, "", "list", "--full")
	if err != nil {
		t.Fatal(err)
	}
	short, err := frictionCLI(t, contextFile, "", "list")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tools list: %d bytes with --full, %d by default", len(full), len(short))
	if len(short) >= 12*1024 {
		t.Fatalf("tools list is %d bytes", len(short))
	}
	var fullListing, shortListing map[string]any
	if err := json.Unmarshal([]byte(full), &fullListing); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(short), &shortListing); err != nil {
		t.Fatal(err)
	}
	everything := append(agentMCPTools(), browserMCPTools()...)
	if got := len(anySlice(fullListing["tools"])); got != len(everything) {
		t.Fatalf("--full listed %d of %d tools", got, len(everything))
	}
	for _, raw := range anySlice(fullListing["tools"]) {
		if mapFromAnyMain(raw)["inputSchema"] == nil {
			t.Fatalf("--full row has no schema: %#v", raw)
		}
	}
	rows := anySlice(shortListing["tools"])
	if len(rows) != len(everything)-1 {
		t.Fatalf("listed %d rows of %d tools with one hidden", len(rows), len(everything))
	}
	for _, raw := range rows {
		row := mapFromAnyMain(raw)
		if len(row) != 2 || fieldString(row, "name") == "" || fieldString(row, "description") == "" {
			t.Fatalf("a listing row is not a name and one sentence: %#v", row)
		}
		description := fieldString(row, "description")
		if len(description) > 110 || strings.Contains(description, ". ") {
			t.Fatalf("%s is not one short sentence: %q", fieldString(row, "name"), description)
		}
		if fieldString(row, "name") == "workass_register_process_child" {
			t.Fatal("an internal tool is in the listing")
		}
	}
	if usage := fieldString(shortListing, "usage"); !strings.Contains(usage, "--full") {
		t.Fatalf("the listing does not say how to get a schema: %q", usage)
	}
	one, err := frictionCLI(t, contextFile, "", "list", "workass_read_chat")
	if err != nil || !strings.Contains(one, `"inputSchema"`) {
		t.Fatalf("one schema: %v %s", err, one)
	}
	// Hidden from the listing, still answerable and still callable: this is the
	// verb `workass run` reports through.
	hidden, err := frictionCLI(t, contextFile, "", "list", "workass_register_process_child")
	if err != nil || !strings.Contains(hidden, `"inputSchema"`) {
		t.Fatalf("internal schema: %v %s", err, hidden)
	}
	if _, err := frictionCLI(t, contextFile, `{}`, "call", "workass_register_process_child"); err == nil || strings.Contains(err.Error(), "unknown Workass tool") {
		t.Fatalf("workass run's tool stopped being callable: %v", err)
	}
	if _, err := frictionCLI(t, contextFile, "", "list", "a", "b"); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("two names: %v", err)
	}
	if _, err := frictionCLI(t, contextFile, "", "list", "--verbose"); err == nil || !strings.Contains(err.Error(), "--full") {
		t.Fatalf("unknown list flag: %v", err)
	}
}

func TestToolFrictionWaitsNeedNoOperationIDAndResolveWhatAgentsPass(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	if _, failure := frictionCall(t, h, "workass_wait_subagents", map[string]any{"timeout_ms": 1000}); !strings.Contains(failure, "no subagents") {
		t.Fatalf("a turn with no subagents: %q", failure)
	}
	spawn := func(operationID, label string) string {
		t.Helper()
		id := toString(frictionMustCall(t, h, "workass_spawn_subagent", map[string]any{
			"operation_id": operationID, "task": "harmless wait fixture", "label": label,
		})["id"])
		if id == "" {
			t.Fatal("spawn returned no id")
		}
		return id
	}
	first, second := spawn("friction-spawn-a", "a"), spawn("friction-spawn-b", "b")
	receipts := func() int {
		state, _ := h.runtime.Snapshot("mcp-chat")
		return len(state.AgentWaitObservationReceipts)
	}

	single := frictionMustCall(t, h, "workass_wait_subagent", map[string]any{"subagent_id": first, "timeout_ms": 8000})
	if fieldString(single, "status") != "done" {
		t.Fatalf("single wait with no operation id: %#v", single)
	}
	if receipts() != 1 {
		t.Fatalf("the wait left %d durable observations", receipts())
	}
	frictionMustCall(t, h, "workass_wait_subagent", map[string]any{"subagent_id": first, "timeout_ms": 8000})
	if receipts() != 1 {
		t.Fatalf("the same wait derived a different id: %d observations", receipts())
	}

	completedIDs := func(result map[string]any) map[string]bool {
		ids := map[string]bool{}
		for _, raw := range anySlice(result["completed"]) {
			ids[fieldString(mapFromAnyMain(raw), "id")] = true
		}
		return ids
	}
	byOperation := frictionMustCall(t, h, "workass_wait_subagents", map[string]any{
		"operation_ids": []any{"friction-spawn-a", "friction-spawn-b"}, "return_when": "all", "timeout_ms": 8000,
	})
	if ids := completedIDs(byOperation); !ids[first] || !ids[second] || byOperation["timedOut"] != false {
		t.Fatalf("operation_ids did not resolve to their subagents: %#v", byOperation)
	}
	mixed := frictionMustCall(t, h, "workass_wait_subagents", map[string]any{
		"subagent_ids": []any{first}, "operation_ids": []any{"friction-spawn-b"}, "return_when": "all", "timeout_ms": 8000,
	})
	if ids := completedIDs(mixed); !ids[first] || !ids[second] {
		t.Fatalf("both selectors together: %#v", mixed)
	}
	everyone := frictionMustCall(t, h, "workass_wait_subagents", map[string]any{"timeout_ms": 8000})
	if ids := completedIDs(everyone); !ids[first] || !ids[second] {
		t.Fatalf("omitted ids did not mean this turn's subagents: %#v", everyone)
	}
	explicit := frictionMustCall(t, h, "workass_wait_subagents", map[string]any{"subagent_ids": []any{first, second}, "return_when": "all", "timeout_ms": 8000})
	if ids := completedIDs(explicit); !ids[first] || !ids[second] {
		t.Fatalf("explicit ids: %#v", explicit)
	}
	before := receipts()
	frictionMustCall(t, h, "workass_wait_subagents", map[string]any{"subagent_ids": []any{first, second}, "return_when": "all", "timeout_ms": 8000})
	if receipts() != before {
		t.Fatal("repeating an identical wait is not one durable observation")
	}

	if _, failure := frictionCall(t, h, "workass_wait_subagents", map[string]any{"operation_ids": []any{"never-spawned"}, "timeout_ms": 1000}); !strings.Contains(failure, "never-spawned") || !strings.Contains(failure, "workass_list_subagents") {
		t.Fatalf("an unknown operation id: %q", failure)
	}
	// A caller's own id keeps its contract: reuse for a different wait is refused.
	frictionMustCall(t, h, "workass_wait_subagents", map[string]any{"operation_id": "friction-own-wait", "subagent_ids": []any{first}, "timeout_ms": 8000})
	if _, failure := frictionCall(t, h, "workass_wait_subagents", map[string]any{"operation_id": "friction-own-wait", "subagent_ids": []any{second}, "timeout_ms": 8000}); !strings.Contains(failure, "reused") {
		t.Fatalf("a reused wait id: %q", failure)
	}
}

func TestToolFrictionWaitForAllHoldsUntilARunningChildFinishes(t *testing.T) {
	// Released by a file, which the mock finds whenever it gets there. A message could
	// come before the child had a turn, and then would wait for that turn to end.
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("WORKASS_MOCK_ACP_HOLD_FILE", release)
	h := newStatelessMCPTestHarness(t)
	frictionMustCall(t, h, "workass_spawn_subagent", map[string]any{
		"operation_id": "friction-held", "task": "[mock:hold-until-steer] harmless held fixture", "label": "held",
	})
	window := frictionMustCall(t, h, "workass_wait_subagents", map[string]any{"timeout_ms": 1000})
	if window["timedOut"] != true || len(anySlice(window["running"])) != 1 {
		t.Fatalf("omitted ids did not wait on the running child: %#v", window)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A wait ends when the child does. How long that takes is the machine's.
	done := frictionMustCall(t, h, "workass_wait_subagents", map[string]any{"timeout_ms": 120000})
	if done["timedOut"] != false || len(anySlice(done["completed"])) != 1 {
		t.Fatalf("the wait did not finish: %#v", done)
	}
}

func TestToolFrictionCallFlagsAndInlineInput(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	contextFile, count := toolCLIContextFixture(t, h.handler)

	read, err := frictionCLI(t, contextFile, "", "call", "workass_read_chat", "--tab_id", "mcp-tab", "--chat_id", "mcp-chat", "--limit", "1", "--include_events", "false")
	if err != nil || !strings.Contains(read, `"chatId":"mcp-chat"`) {
		t.Fatalf("flag pairs on a read: %v %s", err, read)
	}
	// A numeric-looking value for a string argument stays a string, because the
	// schema decides the type and not the look of the text.
	before := count.Load()
	renamed, err := frictionCLI(t, contextFile, "", "call", "workass_rename_chat", "--operation_id", "1234", "--title", "2026")
	if err != nil || !strings.Contains(renamed, `"title":"2026"`) {
		t.Fatalf("flag pairs on a mutation: %v %s", err, renamed)
	}
	if count.Load() != before+1 {
		t.Fatal("flag pairs cost more than one request")
	}

	if _, err := frictionCLI(t, contextFile, "", "call", "workass_read_chat", "--tab_id", "mcp-tab", "--chat_id", "mcp-chat", "--limit", "many"); err == nil || !strings.Contains(err.Error(), "limit must be an integer") {
		t.Fatalf("a wrong type: %v", err)
	}

	for _, unknown := range [][]string{
		{"--bogus", "1"},          // unknown argument, reaches the schema
		{"stray"},                 // positional, refused before any call
		{"--title", "a", "stray"}, // both
	} {
		args := append([]string{"call", "workass_create_subsession"}, unknown...)
		_, err := frictionCLI(t, contextFile, "", args...)
		if err == nil {
			t.Fatalf("%v was accepted", unknown)
		}
		text := err.Error()
		for _, want := range []string{"workass_create_subsession", "operation_id", `"title"`, "--input"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%v: %q does not contain %q", unknown, text, want)
			}
		}
		// The example is the tool's own and works as written.
		start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
		if start < 0 || end <= start {
			t.Fatalf("no example in %q", text)
		}
		var example map[string]any
		if json.Unmarshal([]byte(text[start:end+1]), &example) != nil {
			t.Fatalf("example does not parse: %q", text)
		}
		if err := validateToolArguments(catalogTool(t, "workass_create_subsession"), example); err != nil {
			t.Fatalf("example fails its own schema: %v in %q", err, text)
		}
	}

	if result, err := frictionCLI(t, contextFile, "", "call", "workass_list_chats", "--input", "{}"); err != nil || !strings.Contains(result, "chats") {
		t.Fatalf("--input {}: %v %s", err, result)
	}
	if result, err := frictionCLI(t, contextFile, "", "call", "workass_read_chat", "--input", `{"limit":1}`); err != nil || !strings.Contains(result, "mcp-chat") {
		t.Fatalf("inline JSON: %v %s", err, result)
	}
	if result, err := frictionCLI(t, contextFile, "", "call", "workass_read_chat", `{"limit":1}`); err != nil || !strings.Contains(result, "mcp-chat") {
		t.Fatalf("positional inline JSON: %v %s", err, result)
	}
	if result, err := frictionCLI(t, contextFile, `{"limit":1}`, "call", "workass_read_chat", "--input", "-"); err != nil || !strings.Contains(result, "mcp-chat") {
		t.Fatalf("--input - reads stdin: %v %s", err, result)
	}
	if _, err := frictionCLI(t, contextFile, "", "call", "workass_list_chats", "--input", "/no/such/file.json"); err == nil || !strings.Contains(err.Error(), "cannot open tool arguments file") || !strings.Contains(err.Error(), "inline") {
		t.Fatalf("a missing file: %v", err)
	}
	// A real file wins, whatever it is called, so --input cannot read differently.
	odd := filepath.Join(t.TempDir(), "{odd}.json")
	if err := os.WriteFile(odd, []byte(`{"limit":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if result, err := frictionCLI(t, contextFile, "", "call", "workass_read_chat", "--input", odd); err != nil || !strings.Contains(result, "mcp-chat") {
		t.Fatalf("a file with an odd name: %v %s", err, result)
	}
	// JSON arguments reach the daemon as they always did: never coerced.
	if _, err := frictionCLI(t, contextFile, `{"limit":"1"}`, "call", "workass_read_chat"); err == nil || !strings.Contains(err.Error(), "limit must be an integer") {
		t.Fatalf("a JSON string was coerced: %v", err)
	}
	// Flags never read stdin, so a shell with stdin held open cannot hang.
	var output, diagnostics bytes.Buffer
	if err := runToolsCommand(context.Background(), []string{"--context", contextFile, "call", "workass_read_chat", "--limit", "1"}, stdinNotRead{}, &output, &diagnostics); err != nil {
		t.Fatalf("flag pairs read stdin: %v", err)
	}
}

type stdinNotRead struct{}

func (stdinNotRead) Read([]byte) (int, error) { return 0, errors.New("stdin was read") }

func TestToolFrictionUpdateErrorsSayWhatIsWrong(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	h.handler.agentControl.selfMachineID = "m-here"
	h.handler.agentControl.remoteChats = &stubAgentChatRemoteRouter{err: errNoLocalRenderer}

	status := frictionMustCall(t, h, "workass_get_update_status", map[string]any{})
	if fieldString(status, "machineId") != "m-here" || status["daemonOnly"] != true {
		t.Fatalf("status with no machine_id is this machine: %#v", status)
	}
	frictionMustCall(t, h, "workass_get_update_status", map[string]any{"machine_id": "m-here"})

	apply := map[string]any{
		"operation_id": "friction-apply", "machine_id": "m-here", "expected_current_version": "0.1.1",
		"expected_target_version": "0.1.2",
	}
	for name, arguments := range map[string]map[string]any{
		"workass_apply_update":        apply,
		"workass_list_update_targets": {},
		"workass_get_update_status":   {"machine_id": "m-other"},
	} {
		_, failure := frictionCall(t, h, name, arguments)
		if failure == "" {
			t.Fatalf("%s reported success", name)
		}
		if strings.Contains(failure, "router") || strings.Contains(failure, "MCP") {
			t.Fatalf("%s names plumbing: %q", name, failure)
		}
		if !strings.Contains(failure, "window") || !strings.Contains(strings.ToLower(failure), "open") {
			t.Fatalf("%s does not say what to do: %q", name, failure)
		}
	}
	_, failure := frictionCall(t, h, "workass_get_update_status", map[string]any{"machine_id": "m-other"})
	if !strings.Contains(failure, "m-other") || strings.Contains(failure, "is connected") {
		t.Fatalf("a dead local relay blamed the other machine: %q", failure)
	}
	// A refusal that really did come from the other machine still says so.
	if err := updateStatusPeerRefusal("m-other", errors.New(errNoLocalRenderer.Error())); err == nil || !strings.Contains(err.Error(), "is connected") {
		t.Fatalf("peer refusal: %v", err)
	}

	bad := copyAnyMap(apply)
	bad["expected_target_version"] = "latest"
	if _, failure := frictionCall(t, h, "workass_apply_update", bad); !strings.Contains(failure, "workass_get_update_status") {
		t.Fatalf("a wrong version does not say where to read it: %q", failure)
	}
}

func TestToolFrictionMessageToAFinishedSubagentNamesTheNextStep(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	id := toString(frictionMustCall(t, h, "workass_spawn_subagent", map[string]any{
		"operation_id": "friction-finish", "task": "harmless finished fixture", "label": "finished",
	})["id"])
	frictionMustCall(t, h, "workass_wait_subagent", map[string]any{"subagent_id": id, "timeout_ms": 8000})
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, failure := frictionCall(t, h, "workass_message_subagent", map[string]any{"operation_id": fmt.Sprintf("friction-late-%d", time.Now().UnixNano()), "subagent_id": id, "message": "one more thing"})
		if failure != "" {
			for _, want := range []string{"workass_retry_subagent", "workass_wait_subagent"} {
				if !strings.Contains(failure, want) {
					t.Fatalf("no next step in %q", failure)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a finished subagent kept accepting messages")
		}
	}
	if _, failure := frictionCall(t, h, "workass_message_subagent", map[string]any{"operation_id": "friction-unknown", "subagent_id": "no-such-child", "message": "hello"}); !strings.Contains(failure, "workass_list_subagents") {
		t.Fatalf("an unknown id gives no next step: %q", failure)
	}
}
