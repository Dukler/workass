package chat

import (
	"encoding/json"
	"strings"
	"testing"
	"workass/internal/provider"
)

func TestModelAdmissionFailurePreservesInputAndThreadWithoutConnectionFailure(t *testing.T) {
	for _, kind := range []provider.ErrorKind{provider.ErrorModelUnavailable, provider.ErrorAdmissionRejected, provider.ErrorAuthenticationRequired, provider.ErrorTransientTransport} {
		t.Run(string(kind), func(t *testing.T) {
			state, first, _ := deltaFixture(t)
			thread := state.Lanes[first.ID].Thread
			state, _ = submitDelta(t, state, "keep-this-request")
			state, effects := apply(t, state, TurnAdmissionFailed{OperationID: "keep-this-request", Kind: kind})
			if len(effects) != 0 || state.Foreground != nil || len(state.Queue) != 0 {
				t.Fatal("rejection resent or queued the input")
			}
			if state.Lanes[first.ID].Thread != thread {
				t.Fatal("rejection changed native history")
			}
			user, assistant := state.Ledger[len(state.Ledger)-2], state.Ledger[len(state.Ledger)-1]
			if user.Text != "keep-this-request" || assistant.Status != "failed" {
				t.Fatal("lost the rejected turn")
			}
			if assistant.Interrupted != (kind == provider.ErrorTransientTransport) || assistant.Terminal.Interrupted != assistant.Interrupted {
				t.Fatal("incorrect connection-loss presentation")
			}
			if kind == provider.ErrorModelUnavailable && !strings.Contains(assistant.Text, "Choose an available model") {
				t.Fatal("missing actionable explanation")
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			var restored State
			if err = json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if err = restored.Validate(); err != nil {
				t.Fatal(err)
			}
			if restored.Ledger[len(restored.Ledger)-1].Interrupted != assistant.Interrupted {
				t.Fatal("restart changed failure classification")
			}
		})
	}
}
