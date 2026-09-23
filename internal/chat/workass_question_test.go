package chat

import (
	"encoding/json"
	"path/filepath"
	"testing"

	providercontract "workass/internal/provider"
)

func TestWorkassQuestionReducerPersistsStructuredAnswerBeforeResolution(t *testing.T) {
	question := providercontract.PermissionQuestion{
		WorkassTool: true, ID: "deployment", OperationID: "ask-deployment",
		Question: "Choose a target", MultiSelect: true, AllowFreeText: true,
		Options: []providercontract.PermissionQuestionOption{
			{ID: "canary", Label: "Canary"}, {ID: "production", Label: "Production"},
		},
	}
	state, _ := NewState("question-chat")
	state.Permissions["question-request"] = PermissionState{
		Owner: ProviderActivityOwner{LaneID: "question-lane", TurnID: "question-turn"},
		Event: providercontract.PermissionEvent{
			RequestID: "question-request", Status: "pending", Options: []string{"canary", "production"}, Question: &question,
		},
	}
	answer := providercontract.QuestionAnswer{
		Status: "answered", SelectedOptionIDs: []string{"production", "canary"}, FreeText: " Keep this exactly. ",
	}
	token, err := providercontract.EncodeWorkassQuestionAnswer(answer)
	if err != nil {
		t.Fatal(err)
	}
	effects, err := reduceResolvePermission(&state, ResolvePermission{
		OperationID: "resolve-workass-question", RequestID: "question-request", OptionID: token,
	})
	if err != nil || len(effects) != 1 {
		t.Fatalf("resolve Workass question = effects %#v err=%v", effects, err)
	}
	stored := state.Permissions["question-request"]
	if stored.Event.Status != "resolving" || stored.Event.Question == nil || stored.Event.Question.Answer == nil {
		t.Fatalf("question answer was not journaled before dispatch: %#v", stored.Event)
	}
	got := stored.Event.Question.Answer
	if got.Status != answer.Status || got.FreeText != answer.FreeText || len(got.SelectedOptionIDs) != 2 ||
		got.SelectedOptionIDs[0] != "production" || got.SelectedOptionIDs[1] != "canary" {
		t.Fatalf("durable structured answer changed: %#v", got)
	}

	before := state.Clone()
	malformed := "workass-question-v1:%%%"
	if _, err := reduceResolvePermission(&state, ResolvePermission{
		OperationID: "resolve-malformed-question", RequestID: "question-request", OptionID: malformed,
	}); err == nil {
		t.Fatal("malformed question answer reached the permission effect")
	}
	if state.Permissions["question-request"].Event.Question.Answer.FreeText != before.Permissions["question-request"].Event.Question.Answer.FreeText {
		t.Fatal("malformed answer changed the durable question receipt")
	}
}

func TestExternalQuestionMutationReceiptDurablyStoresItsResult(t *testing.T) {
	state, _ := NewState("receipt-chat")
	state.Initialized = true
	state.Presentation.TabID = "receipt-tab"
	const operationID providercontract.OperationID = "ask-once"
	const digest = "0123456789abcdef"
	state.Outbox = append(state.Outbox, OutboxEntry{
		ID: externalMutationEffectID(operationID), Kind: EffectExternalMutation, Status: OutboxDispatched,
		OperationID: operationID, ChatID: "receipt-chat", TabID: "receipt-tab",
		MutationKind: "workass_ask_user_question", MutationMethod: "question.ask", MutationDigest: digest,
	})
	result := json.RawMessage(`{"status":"answered","question_id":"deployment","selected_options":[{"id":"canary","label":"Canary"}],"free_text":"exact","operation_id":"ask-once"}`)
	if err := reduceExternalMutationReceipt(&state, ExternalMutationReceipt{
		OperationID: operationID, Kind: "workass_ask_user_question", Method: "question.ask",
		TabID: "receipt-tab", Digest: digest, Result: result,
	}); err != nil {
		t.Fatal(err)
	}
	entry := externalMutationEntryForOperation(state, operationID)
	if entry == nil || entry.Status != OutboxCompleted || string(entry.Result) != string(result) {
		t.Fatalf("question result receipt = %#v", entry)
	}
	if err := reduceExternalMutationReceipt(&state, ExternalMutationReceipt{
		OperationID: operationID, Kind: "workass_ask_user_question", Method: "question.ask",
		TabID: "receipt-tab", Digest: digest, Result: json.RawMessage(`{"status":"cancelled"}`),
	}); err == nil {
		t.Fatal("completed question receipt changed its answer")
	}
}

func TestWorkassQuestionTerminalStatusesSurviveActorRestartRecovery(t *testing.T) {
	for _, status := range []string{"answered", "dismissed", "cancelled", "timed_out"} {
		t.Run(status, func(t *testing.T) {
			state, _ := NewState("question-recovery-chat")
			lane := testLane("question-recovery-chat", "mock")
			state, _ = apply(t, state, SelectLane{Identity: lane})
			state, _ = apply(t, state, LaneOpened{
				LaneID: lane.ID, Thread: providercontract.ThreadRef{ProviderID: "mock", RootID: "thread", HeadID: "thread", Lineage: 1},
				ConnectionGeneration: 1, Context: exactContext(providercontract.ContextImportUnsupported),
			})
			state, _ = apply(t, state, Submit{OperationID: "recovery-turn", Text: "work", Presentation: providercontract.TurnPresentation{
				UserMessageID: "recovery-user", AssistantMessageID: "recovery-assistant", Origin: "human",
			}})
			state, _ = apply(t, state, TurnAdmitted{OperationID: "recovery-turn", Accepted: true,
				Turn: providercontract.TurnRef{OperationID: "recovery-turn", NativeID: "native-turn"}})
			question := providercontract.PermissionQuestion{
				WorkassTool: true, ID: "recovery-question", OperationID: "ask-recovery", Question: "Choose", AllowFreeText: true,
				Options: []providercontract.PermissionQuestionOption{{ID: "yes", Label: "Yes"}},
			}
			request := providercontract.Event{Kind: providercontract.EventPermissionRequested, Identity: providercontract.EventIdentity{
				ChatID: state.ChatID, LaneID: lane.ID, OperationID: "recovery-turn", TurnID: "native-turn", Sequence: 1,
			}, Permission: &providercontract.PermissionEvent{RequestID: "recovery-request", Status: "pending", Options: []string{"yes"}, Question: &question}}
			state, _ = apply(t, state, ProviderEventReceived{ConnectionGeneration: 1, Event: request})
			if status == "answered" || status == "dismissed" {
				answer := providercontract.QuestionAnswer{Status: status}
				if status == "answered" {
					answer.SelectedOptionIDs = []string{"yes"}
					answer.FreeText = "選択🙂"
				}
				token, err := providercontract.EncodeWorkassQuestionAnswer(answer)
				if err != nil {
					t.Fatal(err)
				}
				state, _ = apply(t, state, ResolvePermission{OperationID: "resolve-recovery-answer", RequestID: "recovery-request", OptionID: token})
			}
			terminalAnswer := &providercontract.QuestionAnswer{Status: status}
			terminal := providercontract.Event{Kind: providercontract.EventPermissionResolved, Identity: providercontract.EventIdentity{
				ChatID: state.ChatID, LaneID: lane.ID, OperationID: "recovery-turn", TurnID: "native-turn", Sequence: 2,
			}, Permission: &providercontract.PermissionEvent{
				RequestID: "recovery-request", Status: "resolved", ResolvedOptionID: providercontract.WorkassQuestionResolvedOptionID,
				Question: &providercontract.PermissionQuestion{WorkassTool: true, ID: question.ID, OperationID: question.OperationID, Answer: terminalAnswer},
			}}
			state, _ = apply(t, state, ProviderEventReceived{ConnectionGeneration: 1, Event: terminal})
			store := FileStore{Path: filepath.Join(t.TempDir(), "actor.json")}
			if err := store.Save(state); err != nil {
				t.Fatalf("persist resolved question actor: %v", err)
			}
			restored, ok, err := store.Load(state.ChatID)
			if err != nil || !ok {
				t.Fatalf("restore resolved question actor: ok=%v err=%v", ok, err)
			}
			resolved := restored.Permissions["recovery-request"].Event.Question
			if resolved == nil || resolved.Answer == nil || resolved.Answer.Status != status {
				t.Fatalf("terminal status was lost after actor restart: %#v", resolved)
			}
			if status == "answered" && (resolved.Answer.FreeText != "選択🙂" || len(resolved.Answer.SelectedOptionIDs) != 1 || resolved.Answer.SelectedOptionIDs[0] != "yes") {
				t.Fatalf("terminal event overwrote the actual answer: %#v", resolved.Answer)
			}
		})
	}
}
