package acp

import "testing"

func TestProviderNotificationAdaptersIsolateVendorFrames(t *testing.T) {
	claudeFrame := map[string]any{
		"sessionUpdate":       "_workass_claude_steer_consumed",
		"clientUserMessageId": "claude-message",
	}
	codexFrame := map[string]any{
		"sessionUpdate":       "_workass_codex_steer_consumed",
		"clientUserMessageId": "codex-message",
	}

	generic := providerAdapterForID("custom").notifications
	if _, ok := generic.Decode(claudeFrame, nil); ok {
		t.Fatal("generic ACP notification adapter consumed a vendor frame")
	}
	if _, ok := generic.Decode(codexFrame, nil); ok {
		t.Fatal("generic ACP notification adapter consumed a vendor frame")
	}

	claude := providerAdapterForID("claude").notifications
	if _, ok := claude.Decode(codexFrame, nil); ok {
		t.Fatal("Claude notification adapter consumed a Codex frame")
	}
	codex := providerAdapterForID("codex").notifications
	if _, ok := codex.Decode(claudeFrame, nil); ok {
		t.Fatal("Codex notification adapter consumed a Claude frame")
	}
	notification, ok := codex.Decode(codexFrame, nil)
	if !ok || notification.Kind != providerNotificationSteerConsumed || notification.SteerConsumed == nil || notification.SteerConsumed.ClientUserMessageID != "codex-message" {
		t.Fatalf("Codex steer frame = %#v, recognized=%v", notification, ok)
	}
}

func TestClaudeNotificationAdapterProducesTypedLifecycleEvents(t *testing.T) {
	strategy := providerAdapterForID("claude").notifications

	tests := []struct {
		name  string
		frame map[string]any
		check func(*testing.T, providerNotification)
	}{
		{
			name: "spawned work",
			frame: map[string]any{
				"sessionUpdate": "_workass_claude_spawned_work",
				"event": map[string]any{
					"type": "progress", "taskId": "task-7", "status": "running", "summary": "still working",
				},
			},
			check: func(t *testing.T, got providerNotification) {
				if got.Kind != providerNotificationSpawnedWork || got.SpawnedWork == nil || got.SpawnedWork.Task.TaskID != "task-7" || got.SpawnedWork.Task.Summary != "still working" {
					t.Fatalf("spawned-work notification = %#v", got)
				}
			},
		},
		{
			name: "harness end",
			frame: map[string]any{
				"sessionUpdate": "_workass_claude_turn", "phase": "ended", "promptId": "prompt-3",
				"harnessEvidence": true,
				"backgroundTasks": []any{map[string]any{"id": "task-3", "type": "bash", "status": "running"}},
				"sessionCrons":    []any{},
			},
			check: func(t *testing.T, got providerNotification) {
				if got.Kind != providerNotificationHarnessTurn || got.HarnessTurn == nil || got.HarnessTurn.Evidence == nil || !got.HarnessTurn.Evidence.Complete() || len(got.HarnessTurn.Evidence.Tasks) != 1 {
					t.Fatalf("harness notification = %#v", got)
				}
			},
		},
		{
			name: "command catalog",
			frame: map[string]any{
				"sessionUpdate":  "_workass_claude_commands",
				"commandCatalog": map[string]any{"commands": []any{map[string]any{"name": "review"}}},
			},
			check: func(t *testing.T, got providerNotification) {
				if got.Kind != providerNotificationCommandCatalog || !got.CatalogSet || got.CommandCatalog == nil || len(got.CommandCatalog.Commands) != 1 || got.CommandCatalog.Commands[0].Name != "review" {
					t.Fatalf("catalog notification = %#v", got)
				}
			},
		},
		{
			name: "lineage",
			frame: map[string]any{
				"sessionUpdate": "_workass_claude_provider_session", "previousProviderSessionId": "thread-1",
				"providerSessionId": "thread-2", "lineageGeneration": float64(4), "lineageProof": "proof",
			},
			check: func(t *testing.T, got providerNotification) {
				if got.Kind != providerNotificationLineage || got.Lineage == nil || got.Lineage.PreviousThreadID != "thread-1" || got.Lineage.ThreadID != "thread-2" || got.Lineage.Generation != 4 || got.Lineage.Proof != "proof" {
					t.Fatalf("lineage notification = %#v", got)
				}
			},
		},
		{
			name: "heartbeat",
			frame: map[string]any{
				"sessionUpdate": "_workass_claude_turn_heartbeat", "elapsedMs": float64(1250), "outputTokens": float64(42),
				"phase": "tool", "toolName": "Bash", "retry": map[string]any{"code": float64(529), "attempt": float64(2)},
			},
			check: func(t *testing.T, got providerNotification) {
				pulse := got.Heartbeat
				if got.Kind != providerNotificationHeartbeat || pulse == nil || !pulse.ElapsedMSSet || pulse.ElapsedMS != 1250 || !pulse.OutputTokensSet || pulse.OutputTokens != 42 || pulse.Retry == nil || !pulse.Retry.CodeSet || pulse.Retry.Code != 529 || !pulse.Retry.AttemptSet || pulse.Retry.Attempt != 2 {
					t.Fatalf("heartbeat notification = %#v", got)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := strategy.Decode(test.frame, nil)
			if !ok {
				t.Fatalf("registered frame was not recognized: %#v", test.frame)
			}
			test.check(t, got)
		})
	}
}

func TestClaudeNotificationAdapterConsumesMalformedPrivateFramesSafely(t *testing.T) {
	strategy := providerAdapterForID("claude").notifications
	got, ok := strategy.Decode(map[string]any{
		"sessionUpdate": "_workass_claude_spawned_work",
		"event":         "not-an-object",
	}, nil)
	if !ok || got.Kind != providerNotificationSpawnedWork || got.SpawnedWork != nil {
		t.Fatalf("malformed private frame escaped its adapter: %#v, recognized=%v", got, ok)
	}
	if _, ok := strategy.Decode(map[string]any{"sessionUpdate": "agent_message_chunk"}, nil); ok {
		t.Fatal("standard ACP notification was consumed by the provider adapter")
	}
}
