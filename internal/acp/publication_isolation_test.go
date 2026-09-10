package acp

import (
	"sync"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

// A stopped provider has already returned its terminal result. Saving or
// broadcasting another chat must not keep that result behind a manager-wide
// publication lock (the durable actor ACK can take up to InitTimeout).
func TestTerminalPublicationDoesNotWaitForAnotherChat(t *testing.T) {
	for _, blockedAt := range []string{"actor-commit", "broadcast"} {
		t.Run(blockedAt, func(t *testing.T) {
			blocked := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			terminalPublished := make(chan struct{}, 1)
			manager := NewManager(Options{
				RSSSampleInterval: time.Hour,
				Broadcast: func(channel string, raw any) {
					if channel != "job:event" {
						return
					}
					payload := mapFromAny(raw)
					if asString(payload["id"]) == "slow-job" && blockedAt == "broadcast" {
						close(blocked)
						<-release
					}
					if asString(mapFromAny(payload["job"])["id"]) == "stopped-job" {
						terminalPublished <- struct{}{}
					}
				},
			})
			defer manager.Reset()
			slow := newUnopenedManagerLaneForTest(t, manager, "slow-chat", "slow-session")
			stopped := newUnopenedManagerLaneForTest(t, manager, "stopped-chat", "stopped-session")
			for _, lane := range []*managerLane{slow, stopped} {
				<-lane.Events()
				lane.RequireDurableEventCommits()
			}
			manager.bindProviderLaneJob(slow, "slow-job", "slow-operation")
			manager.bindProviderLaneJob(stopped, "stopped-job", "stopped-operation")
			var forwarders sync.WaitGroup
			for _, lane := range []*managerLane{slow, stopped} {
				forwarders.Add(1)
				go func(lane *managerLane) {
					defer forwarders.Done()
					for event := range lane.Events() {
						if lane == slow && event.Kind == providercontract.EventAssistantChunk && blockedAt == "actor-commit" {
							close(blocked)
							<-release
						}
						lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
					}
				}(lane)
			}
			var emissions sync.WaitGroup
			defer func() {
				unblock()
				emissions.Wait()
				slow.attachmentClosed()
				stopped.attachmentClosed()
				forwarders.Wait()
			}()
			emissions.Add(1)
			go func() {
				defer emissions.Done()
				manager.emit("job:event", map[string]any{"type": "data", "id": "slow-job", "stream": "stdout", "chunk": "fixture"})
			}()
			select {
			case <-blocked:
			case <-time.After(time.Second):
				t.Fatal("slow chat did not reach the blocked boundary")
			}
			emissions.Add(1)
			go func() {
				defer emissions.Done()
				manager.emit("job:event", map[string]any{"type": "end", "job": map[string]any{
					"id": "stopped-job", "sessionId": "stopped-session", "status": "done", "stopReason": "cancelled", "code": 130,
				}})
			}()
			select {
			case <-terminalPublished:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("native terminal result waited behind another chat's " + blockedAt)
			}
			unblock()
			emissions.Wait()
			manager.providerPublicationMu.Lock()
			retained := len(manager.providerPublications)
			manager.providerPublicationMu.Unlock()
			if retained != 0 {
				t.Fatal("completed publications retained historical chat locks")
			}
		})
	}
}

func TestPublicationOrderingSurvivesDifferentAttachmentsOfSameChat(t *testing.T) {
	firstEntered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	secondPublished := make(chan struct{}, 1)
	manager := NewManager(Options{RSSSampleInterval: time.Hour, Broadcast: func(_ string, raw any) {
		switch asString(mapFromAny(raw)["id"]) {
		case "first-job":
			close(firstEntered)
			<-release
		case "second-job":
			secondPublished <- struct{}{}
		}
	}})
	defer manager.Reset()
	var workers sync.WaitGroup
	lanes := []*managerLane{
		newUnopenedManagerLaneForTest(t, manager, "same-chat", "first-session"),
		newUnopenedManagerLaneForTest(t, manager, "same-chat", "second-session"),
	}
	for _, lane := range lanes {
		<-lane.Events()
		workers.Add(1)
		go func(lane *managerLane) {
			defer workers.Done()
			for event := range lane.Events() {
				lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
			}
		}(lane)
	}
	manager.bindProviderLaneJob(lanes[0], "first-job", "first-operation")
	manager.bindProviderLaneJob(lanes[1], "second-job", "second-operation")
	var emissions sync.WaitGroup
	defer func() {
		unblock()
		emissions.Wait()
		for _, lane := range lanes {
			lane.attachmentClosed()
		}
		workers.Wait()
	}()
	emit := func(job string) {
		emissions.Add(1)
		go func() {
			defer emissions.Done()
			manager.emit("job:event", map[string]any{"type": "data", "id": job, "stream": "stdout", "chunk": "fixture"})
		}()
	}
	emit("first-job")
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first attachment did not enter publication")
	}
	emit("second-job")
	select {
	case <-secondPublished:
		t.Fatal("a different attachment reordered the same chat's publications")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case <-secondPublished:
	case <-time.After(time.Second):
		t.Fatal("second publication did not finish after the first")
	}
}
