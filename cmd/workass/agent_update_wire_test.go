package main

import (
	"strings"
	"testing"

	"workass/internal/wire"
)

func TestAgentUpdateWireRoutesOnlyTheExactMachine(t *testing.T) {
	router := &stubAgentChatRemoteRouter{result: map[string]any{"ok": true}}
	hub := wire.NewHub()
	registerAgentUpdateWireHandlers(hub, router, "machine-san")

	result, err := hub.Invoke(agentUpdateStatusChannel, []any{map[string]any{"machine_id": "machine-san"}})
	if err != nil || mapFromAnyMain(result)["ok"] != true {
		t.Fatalf("status result=%#v err=%v", result, err)
	}
	if router.method != "update.local.status" || fieldString(router.params, "machine_id") != "machine-san" {
		t.Fatalf("status route method=%q params=%#v", router.method, router.params)
	}
	if _, err := hub.Invoke(agentUpdateStatusChannel, []any{map[string]any{"machine_id": "machine-other"}}); err == nil || !strings.Contains(err.Error(), "exact") {
		t.Fatalf("wrong-machine status error=%v", err)
	}
}

func TestAgentUpdateWireFencesAuthorizedApplyBeforeRenderer(t *testing.T) {
	router := &stubAgentChatRemoteRouter{result: map[string]any{"started": true}}
	hub := wire.NewHub()
	registerAgentUpdateWireHandlers(hub, router, "machine-san")
	valid := map[string]any{
		"machine_id": "machine-san", "expected_current_version": "1.2.3", "expected_target_version": "1.2.4",
		"authorization": "update machine-san from 1.2.3 to 1.2.4", "operation_id": "update-san-1.2.4",
	}
	for name, mutate := range map[string]func(map[string]any){
		"wrong machine":            func(params map[string]any) { params["machine_id"] = "machine-other" },
		"loose version":            func(params map[string]any) { params["expected_target_version"] = "v1.2.4" },
		"wrong authorization":      func(params map[string]any) { params["authorization"] = "please update" },
		"missing stable operation": func(params map[string]any) { delete(params, "operation_id") },
	} {
		t.Run(name, func(t *testing.T) {
			params := copyAnyMap(valid)
			mutate(params)
			router.method = ""
			if _, err := hub.Invoke(agentUpdateApplyChannel, []any{params}); err == nil {
				t.Fatalf("unsafe apply was accepted: %#v", params)
			}
			if router.method != "" {
				t.Fatalf("unsafe apply reached renderer through %q", router.method)
			}
		})
	}
	result, err := hub.Invoke(agentUpdateApplyChannel, []any{valid})
	if err != nil || mapFromAnyMain(result)["started"] != true {
		t.Fatalf("valid apply result=%#v err=%v", result, err)
	}
	if router.method != "update.local.apply" || fieldString(router.params, "operation_id") != "update-san-1.2.4" {
		t.Fatalf("apply route method=%q params=%#v", router.method, router.params)
	}
}
