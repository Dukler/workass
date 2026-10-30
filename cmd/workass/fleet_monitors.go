package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"workass/internal/acp"
	"workass/internal/fleetclient"
	"workass/internal/fleetlog"
	"workass/internal/machineid"
	"workass/internal/wire"
)

// Wiring P3 into the daemon: two registries, one log directory, and the four
// additive channels that carry them.
//
// Everything here is optional in the strict sense: an install with no machine
// id, no state directory or an unreadable registry keeps exactly the P2
// behaviour — commands travel, nothing reports back — and says so in the log.
// A monitor is not a feature that can be half-present, so it is either wired or
// it is absent and named.

// fleetMonitorService binds the two halves to the daemon's lifetime.
type fleetMonitorService struct {
	home    *fleetHome
	watcher *fleetWatcher
	logs    *fleetlog.Store
	logger  *log.Logger

	run sync.WaitGroup
}

// openFleetMonitors builds this daemon's log store, its two monitor registries,
// and connects them to the mesh that already exists.
//
// The order is forced and worth stating: the HELD registry is opened first,
// because the log's retention sweep has to ask it what is still owed before it
// may evict a terminal event — "nothing is forgotten before it is consumed"
// outranks the byte cap (§5, §16.3c).
func openFleetMonitors(
	stateDir string,
	identity machineid.Identity,
	hub *wire.Hub,
	mesh *fleetMeshService,
	manager *acp.Manager,
	chats *providerChatRuntime,
	control *agentControlHandler,
	logger *log.Logger,
) *fleetMonitorService {
	service := &fleetMonitorService{logger: logger}
	if strings.TrimSpace(identity.MachineID) == "" {
		logger.Printf("[workass] fleet monitors unavailable: this daemon has no machine id")
		return service
	}
	held, err := fleetlog.OpenMonitors(stateDir, fleetlog.MonitorsHeld)
	if err != nil {
		logger.Printf("[workass] fleet monitors unavailable: %v", err)
		return service
	}
	watched, err := fleetlog.OpenMonitors(stateDir, fleetlog.MonitorsWatched)
	if err != nil {
		logger.Printf("[workass] fleet monitors unavailable: %v", err)
		return service
	}
	logs, err := fleetlog.Open(stateDir, fleetlog.Options{
		Logf: logger.Printf,
		Owed: held.MinAcked,
	})
	if err != nil {
		logger.Printf("[workass] fleet entity logs unavailable: %v", err)
		return service
	}
	service.logs = logs
	service.home = newFleetHome(identity.MachineID, logs, held, hub.SendToPeer, logger.Printf)
	if service.home.Register(hub) {
		logger.Printf("[workass] registered the fleet monitor channels %s, %s, %s, %s",
			fleetMonitorChannel, fleetDemonitorChannel, fleetMonitorSyncChannel, fleetLogAckChannel)
	}
	// Subagents are the first client of the log (§13's M2 order). Every terminal
	// and attention event becomes a log entry here; whether it becomes a notice
	// is the watcher's decision, and for a self-homed watcher that is still the
	// in-process delivery this daemon has always done.
	manager.SetSubagentLifecycleObserver(service.home.ObserveSubagent)
	// Process children are the second client (§13's M2 order). Every terminal,
	// attention and declared stall becomes a log entry here; whether it becomes
	// a notice is the watcher's decision, and for a self-homed owner that is
	// the same decision, reached through the same projection.
	manager.SetProcessLifecycleObserver(service.home.ObserveProcess)

	service.watcher = newFleetWatcher(fleetWatcherOptions{
		Self: identity.MachineID, Monitors: watched, Mesh: mesh.Invoker(),
		Deliver: chats.deliverFleetMonitorNotice, Budget: newFleetNoticeBudget(),
		ChatExists: chats.chatExists, ChatTab: chats.chatTab, Logf: logger.Printf,
	})
	// The home delivers to a watcher on this machine in process, through the
	// watcher's own projection — one path, local and remote.
	service.home.SetLocalDelivery(service.watcher.IngestLocalEvent)
	if service.watcher.RegisterOfferChannel(hub) {
		logger.Printf("[workass] registered the fleet monitor channel %s", fleetMonitorOfferChannel)
	}
	if control != nil {
		control.fleetHome, control.fleetWatch = service.home, service.watcher
		control.fleetMesh = mesh.Invoker()
	}
	mesh.SetMonitorObservers(service.watcher.Ingest, service.watcher.OnLinkState, service.resync)
	if monitors := len(watched.All()); monitors > 0 {
		logger.Printf("[workass] fleet monitors: watching %d entity(ies) on %s", monitors, strings.Join(watched.Homes(), ", "))
	}
	if owed := len(held.All()); owed > 0 {
		logger.Printf("[workass] fleet monitors: %d monitor(s) owed to other machines", owed)
	}
	// Retention is applied once at startup, where it costs nothing and is
	// visible in the log, rather than on a timer of its own.
	logs.Prune()
	return service
}

// resync asks one home for everything this daemon's cursors have not seen. It
// runs because a link came up, never because a timer fired: a four-day outage
// is delivered from the cursor on reconnect, and nothing polled in between
// (§5, §14 "Partition for days").
func (s *fleetMonitorService) resync(ctx context.Context, peerID string) {
	if s == nil || s.watcher == nil {
		return
	}
	s.run.Add(1)
	go func() {
		defer s.run.Done()
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := s.watcher.Resync(callCtx, peerID); err != nil {
			s.logger.Printf("[fleet] could not resync monitors with %s: %v", peerID, err)
		}
	}()
}

// Close joins the resyncs so shutdown does not race a delivery.
func (s *fleetMonitorService) Close() {
	if s == nil {
		return
	}
	s.run.Wait()
}

// chatExists answers whether one chat is still here, which is what lets this
// daemon drop a monitor the home holds for a watcher that is gone. The home
// cannot answer it: a chat on another machine is not its to know.
func (r *providerChatRuntime) chatExists(tabID, chatID string) bool {
	if r == nil {
		return false
	}
	state, ok := r.Snapshot(strings.TrimSpace(chatID))
	if !ok || state.Deleted {
		return false
	}
	if tabID = strings.TrimSpace(tabID); tabID != "" && state.Presentation.TabID != tabID {
		return false
	}
	return true
}

// chatTab is which tab a chat on this machine lives in, for an offer that
// named the chat without one.
func (r *providerChatRuntime) chatTab(chatID string) string {
	if r == nil {
		return ""
	}
	state, ok := r.Snapshot(strings.TrimSpace(chatID))
	if !ok || state.Deleted {
		return ""
	}
	return strings.TrimSpace(state.Presentation.TabID)
}

// watcherOrigin is the asking chat's own lane and running turn, which a monitor
// records so the outcome lands on the right lane and so the "parent turn was
// stopped" suppression can be evaluated HERE.
//
// The host's lane and operation ids are meaningless on this machine — they name
// a lane in the host's chat — so carrying them would resolve to nothing and
// silently disable a suppression condition §7 requires.
func (r *providerChatRuntime) watcherOrigin(tabID, chatID string) (string, string) {
	if r == nil {
		return "", ""
	}
	state, ok := r.Snapshot(strings.TrimSpace(chatID))
	if !ok {
		return "", ""
	}
	if state.Foreground != nil {
		return string(state.Foreground.LaneID), string(state.Foreground.OperationID)
	}
	return string(state.DesiredLaneID), ""
}

// Invoker is the mesh as the watcher uses it: invoke a peer, read link state.
func (s *fleetMeshService) Invoker() fleetMonitorInvoker {
	if s == nil || s.mesh == nil {
		return nil
	}
	return s.mesh
}

// SetMonitorObservers hands the mesh the three callbacks P3 needs. They are set
// after construction because the mesh is opened before the chat runtime and the
// agent control surface exist, and because a mesh with none of them is exactly
// the P2 daemon: events are read off the socket and dropped.
func (s *fleetMeshService) SetMonitorObservers(
	onEvent func(peerID, channel string, payload any),
	onBeat func(fleetclient.State),
	onUp func(ctx context.Context, peerID string),
) {
	if s == nil {
		return
	}
	s.observers.Lock()
	s.onEvent, s.onBeat, s.onUp = onEvent, onBeat, onUp
	s.observers.Unlock()
}

func (s *fleetMeshService) monitorObservers() (func(string, string, any), func(fleetclient.State), func(context.Context, string)) {
	if s == nil {
		return nil, nil, nil
	}
	s.observers.RLock()
	defer s.observers.RUnlock()
	return s.onEvent, s.onBeat, s.onUp
}
