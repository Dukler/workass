package acp

import (
	"context"
	"testing"
)

func TestServiceTierRefusesUnadvertisedControls(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "resume")
	manager, _ := fixture.newManagerTuned(nil)
	t.Cleanup(func() { manager.Reset() })
	info, err := manager.NewSession(context.Background(), SessionOptions{TabID: "speed-tab", ChatID: "speed-chat", ProviderID: "mock", CWD: manager.opts.RootDir})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.applyServiceTier(context.Background(), info.SessionID, ""); err != nil {
		t.Fatal(err)
	}
	if err = manager.applyServiceTier(context.Background(), info.SessionID, "fast"); err == nil {
		t.Fatal("unsupported Fast was silently accepted")
	}
}
