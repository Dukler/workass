//go:build review

// Independent-review reproduction (b8b18322..fe7003e5), inverted to assert the
// fix. Run with:
//   go test -tags review -count=1 -run TestReview ./internal/acp/

package acp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func reviewCatalogEvents(events *eventCollector) []collectedEvent {
	var out []collectedEvent
	for _, event := range events.snapshot() {
		if event.channel == "chat:catalog" {
			out = append(out, event)
		}
	}
	return out
}

func reviewGroupJSON(t *testing.T, group *CatalogGroup) string {
	t.Helper()
	if group == nil {
		t.Fatal("mock catalog group missing")
	}
	raw, err := json.Marshal(group)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// state:digest hashes CatalogSnapshotGroups; the renderer hashes the groups of
// the last chat:catalog it received. A background catalog refresh whose models
// are unchanged still rewrites LatencyMs. It used to publish nothing, so the
// digest diverged while providers:list (the renderer's catalog-scope sync since
// ac5ca324) carried no catalog to heal it. The refresh now publishes whenever
// the digest-visible group changes, and providers:catalog returns the exact
// groups the digest hashes for a renderer that missed the event.
func TestReviewCatalogRefreshDivergesDigestWithoutPublishing(t *testing.T) {
	events := newEventCollector()
	manager := catalogRefreshTestManager(t, events)
	ctx := context.Background()

	// Populate the catalog, then pin a sentinel latency and publish it, as the
	// renderer would have last seen it.
	manager.EmitCatalog(ctx)
	_ = events.waitChannel(t, "chat:catalog", 20*time.Second)
	manager.mu.Lock()
	sentinel := int64(-7)
	manager.providers["mock"].LatencyMs = &sentinel
	manager.mu.Unlock()
	manager.emitCatalogSnapshot()
	rendererView := reviewGroupJSON(t, findCatalogGroup(manager.CatalogSnapshotGroups(), "mock"))
	published := len(reviewCatalogEvents(events))

	// The refresh path updates.go uses after a provider update recheck.
	manager.invalidateProviderCatalog("mock")
	manager.refreshProviderCatalogNow(ctx, "mock")
	daemonView := reviewGroupJSON(t, findCatalogGroup(manager.CatalogSnapshotGroups(), "mock"))
	if daemonView == rendererView {
		t.Fatal("refresh did not change the digest-visible catalog group; nothing to publish")
	}
	after := reviewCatalogEvents(events)
	if len(after) != published+1 {
		t.Fatalf("refresh published %d chat:catalog events, want exactly 1", len(after)-published)
	}
	payload, _ := after[len(after)-1].payload.(map[string]any)
	groups, _ := payload["groups"].([]CatalogGroup)
	if got := reviewGroupJSON(t, findCatalogGroup(groups, "mock")); got != daemonView {
		t.Fatalf("published group differs from the digest-visible one:\n published: %s\n digest:    %s", got, daemonView)
	}

	// providers:list stays a provider read with no catalog side effect.
	_ = manager.ProvidersList()
	if got := len(reviewCatalogEvents(events)); got != published+1 {
		t.Fatalf("providers:list published chat:catalog (%d)", got-published-1)
	}
	// A renderer that missed the event reads providers:catalog: the digest's
	// own groups, with no publication and no probe.
	snapshot := manager.CatalogSnapshot()
	snapshotGroups, _ := snapshot["groups"].([]CatalogGroup)
	if got := reviewGroupJSON(t, findCatalogGroup(snapshotGroups, "mock")); got != daemonView {
		t.Fatalf("providers:catalog group differs from the digest-visible one:\n read:   %s\n digest: %s", got, daemonView)
	}
	if got := len(reviewCatalogEvents(events)); got != published+1 {
		t.Fatalf("providers:catalog published chat:catalog (%d)", got-published-1)
	}
	// The old renderer's catalog-scope sync: providers:detect re-publishes.
	_ = manager.DetectProviders(ctx, DetectOptions{})
	if got := len(reviewCatalogEvents(events)); got == published+1 {
		t.Fatal("providers:detect did not publish chat:catalog either")
	}
}
