//go:build review

// Independent-review reproduction (b8b18322..fe7003e5). Run with:
//   go test -tags review -count=1 -run TestReview ./internal/acp/

package acp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func reviewCatalogEventCount(events *eventCollector) int {
	count := 0
	for _, event := range events.snapshot() {
		if event.channel == "chat:catalog" {
			count++
		}
	}
	return count
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
// are unchanged still rewrites LatencyMs, so the digest diverges while no
// chat:catalog is published. Before ac5ca324 the renderer's catalog-scope sync
// called providers:detect, which re-emitted chat:catalog; providers:list does
// not, so nothing re-synchronises the renderer's catalog hash.
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
	published := reviewCatalogEventCount(events)

	// The refresh path updates.go uses after a provider update recheck.
	manager.invalidateProviderCatalog("mock")
	manager.refreshProviderCatalogNow(ctx, "mock")
	daemonView := reviewGroupJSON(t, findCatalogGroup(manager.CatalogSnapshotGroups(), "mock"))
	if daemonView == rendererView {
		t.Fatal("refresh did not change the digest-visible catalog group; no divergence to report")
	}
	if got := reviewCatalogEventCount(events); got != published {
		t.Fatalf("refresh published %d chat:catalog events; the renderer would heal", got-published)
	}
	t.Logf("digest-visible group changed without chat:catalog:\n renderer: %s\n daemon:   %s", rendererView, daemonView)

	// The new renderer's catalog-scope sync: providers:list. No publication.
	_ = manager.ProvidersList()
	if got := reviewCatalogEventCount(events); got != published {
		t.Fatalf("providers:list published chat:catalog (%d)", got-published)
	}
	// The old renderer's catalog-scope sync: providers:detect re-publishes.
	_ = manager.DetectProviders(ctx, DetectOptions{})
	if got := reviewCatalogEventCount(events); got == published {
		t.Fatal("providers:detect did not publish chat:catalog either")
	}
}
