package acp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	hibernateReasonIdleTTL  = "idle-ttl"
	hibernateReasonSpareTTL = "spare-ttl"
	recycleReasonAge        = "recycle-age"
	recycleReasonRSS        = "recycle-rss"
)

type hibernateCandidate struct {
	bridge       *Bridge
	reason       string
	ttl          time.Duration
	lastActivity time.Time
	now          time.Time
}

func (m *Manager) lifecycleLoop() {
	ticker := time.NewTicker(m.opts.LifecycleCheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		m.SweepLifecycle()
	}
}

func (m *Manager) rssLoop() {
	ticker := time.NewTicker(m.opts.RSSSampleInterval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		m.SampleRSS(ctx)
		cancel()
	}
}

func (m *Manager) spareLoop() {
	ticker := time.NewTicker(m.opts.SpareCheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		m.WarmSpareSessions()
	}
}

func (m *Manager) SweepLifecycle() {
	now := time.Now()
	for _, bridge := range m.allBridges() {
		if candidate, ok := m.hibernateCandidate(bridge, now); ok {
			m.hibernateBridgeIfEligible(candidate.bridge, candidate.reason, candidate.ttl, candidate.lastActivity, candidate.now, nil)
		}
	}
}

func (m *Manager) hibernateCandidate(b *Bridge, now time.Time) (hibernateCandidate, bool) {
	b.mu.Lock()
	if b.closed || b.state == StateHibernated {
		b.mu.Unlock()
		return hibernateCandidate{}, false
	}
	recycleChanged := false
	if !b.startedAt.IsZero() && m.opts.EngineMaxAge > 0 && now.Sub(b.startedAt) >= m.opts.EngineMaxAge {
		recycleChanged = b.markRecycleAtIdleLocked(recycleReasonAge)
	}
	state := b.state
	pinned := b.pinned || b.hasRunningJobLocked()
	spare := b.spare
	lastActivity := b.lastActivity
	recycleAtIdle := b.recycleAtIdle
	recycleReason := b.recycleReason
	tabID := b.tabID
	chatID := b.chatID
	key := b.key
	b.mu.Unlock()

	if recycleChanged {
		m.bridgeChanged(b, "recycle-marked")
	}
	if pinned {
		return hibernateCandidate{}, false
	}
	if spare {
		if m.opts.SpareTTL > 0 && now.Sub(lastActivity) >= m.opts.SpareTTL {
			return hibernateCandidate{bridge: b, reason: hibernateReasonSpareTTL, ttl: m.opts.SpareTTL, lastActivity: lastActivity, now: now}, true
		}
		return hibernateCandidate{}, false
	}
	if state != StateIdle {
		return hibernateCandidate{}, false
	}
	if recycleAtIdle {
		reason := firstNonEmpty(recycleReason, "recycle")
		if m.hasRunningSpawnedWorkForChat(tabID, chatID) {
			m.opts.Logf("acp hibernate aborted", map[string]any{"key": key, "reason": reason, "state": string(state), "pinned": false, "spawnedWork": true})
			return hibernateCandidate{}, false
		}
		return hibernateCandidate{bridge: b, reason: reason, lastActivity: lastActivity, now: now}, true
	}
	if m.opts.HibernateTTL > 0 && now.Sub(lastActivity) >= m.opts.HibernateTTL {
		if m.hasRunningSpawnedWorkForChat(tabID, chatID) {
			m.opts.Logf("acp hibernate aborted", map[string]any{"key": key, "reason": hibernateReasonIdleTTL, "state": string(state), "pinned": false, "spawnedWork": true})
			return hibernateCandidate{}, false
		}
		return hibernateCandidate{bridge: b, reason: hibernateReasonIdleTTL, ttl: m.opts.HibernateTTL, lastActivity: lastActivity, now: now}, true
	}
	return hibernateCandidate{}, false
}

func (m *Manager) hibernateBridgeIfEligible(b *Bridge, reason string, ttl time.Duration, candidateLastActivity time.Time, candidateNow time.Time, beforeRecheck func()) bool {
	if beforeRecheck != nil {
		beforeRecheck()
	}
	now := time.Now()
	if !candidateNow.IsZero() && candidateNow.After(now) {
		now = candidateNow
	}

	spawnedWork := false
	if reason != hibernateReasonSpareTTL {
		b.mu.Lock()
		tabID := b.tabID
		chatID := b.chatID
		b.mu.Unlock()
		spawnedWork = m.hasRunningSpawnedWorkForChat(tabID, chatID)
	}

	b.mu.Lock()
	if b.closed || b.state == StateHibernated {
		b.mu.Unlock()
		return false
	}
	if b.pinned || b.hasRunningJobLocked() {
		b.mu.Unlock()
		m.opts.Logf("acp hibernate aborted", map[string]any{"key": b.key, "reason": reason, "state": string(b.state), "pinned": true})
		return false
	}
	if spawnedWork {
		key := b.key
		state := b.state
		b.mu.Unlock()
		m.opts.Logf("acp hibernate aborted", map[string]any{"key": key, "reason": reason, "state": string(state), "pinned": false, "spawnedWork": true})
		return false
	}
	switch reason {
	case hibernateReasonIdleTTL:
		if b.state != StateIdle || !b.lastActivity.Equal(candidateLastActivity) || now.Sub(b.lastActivity) < ttl {
			b.mu.Unlock()
			m.opts.Logf("acp hibernate aborted", map[string]any{"key": b.key, "reason": reason, "state": string(b.state), "pinned": false})
			return false
		}
	case hibernateReasonSpareTTL:
		if !b.spare || !b.lastActivity.Equal(candidateLastActivity) || now.Sub(b.lastActivity) < ttl {
			b.mu.Unlock()
			m.opts.Logf("acp hibernate aborted", map[string]any{"key": b.key, "reason": reason, "state": string(b.state), "pinned": false})
			return false
		}
	default:
		if b.state != StateIdle || !b.recycleAtIdle {
			b.mu.Unlock()
			m.opts.Logf("acp hibernate aborted", map[string]any{"key": b.key, "reason": reason, "state": string(b.state), "pinned": false})
			return false
		}
	}

	child := b.child
	stdin := b.stdin
	pending := b.pending
	sessions := make([]string, 0, len(b.sessions))
	for sessionID := range b.sessions {
		sessions = append(sessions, sessionID)
	}
	pid := 0
	if child != nil && child.Process != nil {
		pid = child.Process.Pid
	}
	rssKb := b.rssKb
	startedAt := b.startedAt
	spare := b.spare
	tabID := b.tabID
	chatID := b.chatID

	b.pending = make(map[string]*pendingRequest)
	b.child = nil
	b.stdin = nil
	b.initialized = false
	b.state = StateHibernated
	b.pinned = false
	b.finishedAt = now
	b.recycleAtIdle = false
	b.recycleReason = ""
	b.mu.Unlock()

	for _, p := range pending {
		if p.timer != nil {
			p.timer.Stop()
		}
		p.resolve <- rpcResult{err: errors.New("ACP engine hibernated")}
	}
	for _, sessionID := range sessions {
		m.cancelPermissionsForSession(sessionID)
		if spare {
			m.forgetSession(sessionID, b)
		}
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if child != nil && child.Process != nil {
		_ = stopProcessTree(child.Process)
	}
	m.orphanInProcessSpawnedWorkForChat(tabID, chatID, reason)
	if spare {
		m.removeSpareForBridge(b)
	}
	m.opts.Logf("acp engine hibernated", map[string]any{
		"key":      b.key,
		"pid":      nullableInt(pid),
		"reason":   reason,
		"rssKb":    rssKb,
		"uptimeMs": durationSinceMillis(startedAt, now),
	})
	m.bridgeChanged(b, "hibernated:"+reason)
	return true
}

func (m *Manager) SampleRSS(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, bridge := range m.allBridges() {
		pid := bridge.childPID()
		if pid == 0 {
			continue
		}
		rssKb, err := sampleProcessRSS(ctx, pid)
		if err != nil {
			m.opts.Logf("acp rss sample failed", map[string]any{"key": bridge.Key(), "pid": pid, "error": err.Error()})
			continue
		}
		now := time.Now()
		changed := false
		shouldHibernate := false
		var lastActivity time.Time
		var reason string
		bridge.mu.Lock()
		if bridge.rssKb != rssKb {
			bridge.rssKb = rssKb
			changed = true
		}
		if m.opts.EngineMaxRSSKB > 0 && rssKb >= m.opts.EngineMaxRSSKB {
			if bridge.markRecycleAtIdleLocked(recycleReasonRSS) {
				changed = true
			}
		}
		if !bridge.startedAt.IsZero() && m.opts.EngineMaxAge > 0 && now.Sub(bridge.startedAt) >= m.opts.EngineMaxAge {
			if bridge.markRecycleAtIdleLocked(recycleReasonAge) {
				changed = true
			}
		}
		if bridge.state == StateIdle && !bridge.pinned && bridge.recycleAtIdle {
			shouldHibernate = true
			lastActivity = bridge.lastActivity
			reason = firstNonEmpty(bridge.recycleReason, "recycle")
		}
		state := bridge.state
		key := bridge.key
		bridge.mu.Unlock()

		m.opts.Logf("acp engine rss sampled", map[string]any{"key": key, "pid": pid, "rssKb": rssKb, "state": string(state)})
		if changed {
			m.bridgeChanged(bridge, "rss-sampled")
		}
		if shouldHibernate {
			go m.hibernateBridgeIfEligible(bridge, reason, 0, lastActivity, now, nil)
		}
	}
}

func (m *Manager) Processes() []map[string]any {
	var out []map[string]any
	for _, bridge := range m.allBridges() {
		if proc := bridge.processSummary(); proc != nil {
			out = append(out, proc)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri := processRank(asString(out[i]["status"]))
		rj := processRank(asString(out[j]["status"]))
		if ri != rj {
			return ri < rj
		}
		return asString(out[i]["startedAt"]) > asString(out[j]["startedAt"])
	})
	return out
}

func (m *Manager) ReadProcess(id string) map[string]any {
	for _, proc := range m.Processes() {
		if proc["id"] == id {
			out := map[string]any{"ok": true, "output": ""}
			for k, v := range proc {
				out[k] = v
			}
			return out
		}
	}
	return map[string]any{"ok": false, "error": "proceso no encontrado"}
}

func (m *Manager) KillProcess(id string) map[string]any {
	for _, proc := range m.Processes() {
		if proc["id"] == id {
			return map[string]any{"ok": false, "error": "Ese es un motor ACP protegido; cerrá o reiniciá la conversación en vez de matarlo desde acá."}
		}
	}
	return map[string]any{"ok": false, "error": "proceso no encontrado"}
}

func (m *Manager) KillAllProcesses() map[string]any {
	return map[string]any{"ok": true, "stopped": 0}
}

func (m *Manager) bridgeChanged(b *Bridge, reason string) {
	if reason != "rss-sampled" {
		s := b.lifecycleSnapshot()
		m.opts.Logf("acp engine state changed", map[string]any{
			"key":    s.key,
			"state":  string(s.state),
			"pinned": s.pinned,
			"pid":    nullableInt(s.pid),
			"rssKb":  s.rssKb,
			"reason": reason,
		})
	}
	m.emit("proc:changed", map[string]any{"processes": m.Processes()})
}

func (m *Manager) allBridges() []*Bridge {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[*Bridge]struct{}{}
	out := make([]*Bridge, 0, len(m.bridges))
	for _, bridge := range m.bridges {
		if bridge == nil {
			continue
		}
		if _, ok := seen[bridge]; ok {
			continue
		}
		seen[bridge] = struct{}{}
		out = append(out, bridge)
	}
	return out
}

func (m *Manager) WarmSpareSessions() {
	m.mu.Lock()
	target := m.opts.SpareSessions
	for _, providerID := range m.enabledProviderIDsLocked() {
		// One failed prewarm must never become a process respawn loop. Keep at
		// most one launch in flight per provider and trip a circuit breaker on
		// failure. A successful provider detection or explicit toggle resets it.
		if m.spareBlocked[providerID] || m.spareWarming[providerID] > 0 || m.spareCountLocked(providerID) >= target {
			continue
		}
		m.spareWarming[providerID] = 1
		m.spareSeq++
		seq := m.spareSeq
		gen := m.spareGen
		m.mu.Unlock()
		go m.warmOneSpare(providerID, gen, seq)
		m.mu.Lock()
	}
	m.mu.Unlock()
}

func (m *Manager) spareCountLocked(providerID string) int {
	count := 0
	for _, rec := range m.spareSessions {
		if rec.providerID == providerID {
			count++
		}
	}
	return count
}

func (m *Manager) warmOneSpare(providerID string, gen, seq int64) {
	key := fmt.Sprintf("spare-%d-%d", time.Now().UnixMilli(), seq)
	m.mu.Lock()
	ownerKey := m.newAgentOwnerKeyLocked()
	m.mu.Unlock()
	sessionOpts := SessionOptions{BridgeKey: key, ProviderID: providerID, AgentOwnerKey: ownerKey, Spare: true}
	bridge := m.getBridge(sessionOpts)
	timeout := m.opts.InitTimeout * 2
	if timeout < time.Second {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	info, err := bridge.NewSession(ctx, sessionOpts)

	m.mu.Lock()
	if m.spareWarming[providerID] > 0 {
		m.spareWarming[providerID]--
	}
	current := gen == m.spareGen
	blocked := false
	if err == nil && current && info.SessionID != "" {
		delete(m.spareBlocked, providerID)
		m.spareSessions = append(m.spareSessions, spareRecord{info: info, bridgeKey: key, providerID: providerID, createdAt: time.Now(), agentOwnerKey: ownerKey})
	} else if err != nil && current && !m.spareBlocked[providerID] {
		m.spareBlocked[providerID] = true
		blocked = true
	}
	m.mu.Unlock()
	if err != nil {
		bridge.Close(true, err)
		if blocked {
			m.opts.Logf("acp spare warming disabled", map[string]any{
				"provider": providerID,
				"error":    redactSensitiveText(err.Error()),
			})
		}
		return
	}
	if !current {
		bridge.CloseSession(context.Background(), info.SessionID)
	}
}

func (m *Manager) adoptSpareSession(opts SessionOptions) (SessionInfo, bool) {
	if opts.CWD != "" || opts.BridgeKey != "" {
		return SessionInfo{}, false
	}
	m.mu.Lock()
	providerID := normalizeProviderID(opts.ProviderID)
	if providerID == "" {
		var err error
		providerID, err = m.resolveSessionProviderLocked(opts)
		if err != nil {
			m.mu.Unlock()
			return SessionInfo{}, false
		}
		opts.ProviderID = providerID
	}
	for i := 0; i < len(m.spareSessions); i++ {
		rec := m.spareSessions[i]
		if rec.providerID != providerID {
			continue
		}
		m.spareSessions = append(m.spareSessions[:i], m.spareSessions[i+1:]...)
		bridge := m.sessionBridge[rec.info.SessionID]
		if bridge == nil || bridge.Closed() || bridge.Hibernated() {
			i--
			continue
		}
		newKey := m.normalizeBridgeKeyLocked(opts)
		oldKey := bridge.Key()
		if m.bridges[oldKey] == bridge {
			delete(m.bridges, oldKey)
		}
		m.bridges[newKey] = bridge
		m.bindAgentOwnerLocked(rec.agentOwnerKey, opts.ChatID, opts.TabID)
		m.mu.Unlock()

		now := time.Now()
		bridge.mu.Lock()
		bridge.key = newKey
		bridge.procID = "engine-" + safeProcessID(newKey)
		bridge.spare = false
		bridge.tabID = opts.TabID
		bridge.chatID = opts.ChatID
		bridge.lastActivity = now
		bridge.state = StateWarm
		bridge.mu.Unlock()
		m.bridgeChanged(bridge, "spare-adopted")
		if rec.info.CommandCatalog != nil {
			// The spare attached before any chat identity existed, so its
			// command catalog could not be cached or announced then. Adoption
			// is the moment the chat exists — publish the open-time truth.
			m.storeCommandCatalog(opts.TabID, opts.ChatID, rec.info.SessionID, rec.info.CommandCatalog)
		}
		return rec.info, true
	}
	m.mu.Unlock()
	return SessionInfo{}, false
}

func (m *Manager) removeSpareForBridge(bridge *Bridge) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.spareSessions[:0]
	for _, rec := range m.spareSessions {
		if m.sessionBridge[rec.info.SessionID] != bridge {
			out = append(out, rec)
		}
	}
	m.spareSessions = out
}

func (b *Bridge) childPID() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.state == StateHibernated || b.child == nil || b.child.Process == nil {
		return 0
	}
	return b.child.Process.Pid
}

type bridgeLifecycleSnapshot struct {
	key        string
	state      EngineState
	pinned     bool
	pid        int
	rssKb      int
	startedAt  time.Time
	finishedAt time.Time
}

func (b *Bridge) lifecycleSnapshot() bridgeLifecycleSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	pid := 0
	if b.child != nil && b.child.Process != nil {
		pid = b.child.Process.Pid
	}
	return bridgeLifecycleSnapshot{
		key:        b.key,
		state:      b.state,
		pinned:     b.pinned,
		pid:        pid,
		rssKb:      b.rssKb,
		startedAt:  b.startedAt,
		finishedAt: b.finishedAt,
	}
}

func (b *Bridge) processSummary() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	pid := 0
	if b.child != nil && b.child.Process != nil && b.state != StateHibernated {
		pid = b.child.Process.Pid
	}
	if b.startedAt.IsZero() && pid == 0 && b.state == StateWarm {
		return nil
	}
	status := "running"
	if b.closed {
		status = "failed"
	} else if b.state == StateHibernated {
		status = "done"
	}
	lastLine := ""
	if b.recycleAtIdle {
		lastLine = "recycle pending: " + b.recycleReason
	}
	return map[string]any{
		"id":           b.procID,
		"kind":         "engine",
		"label":        firstNonEmpty(b.agentName, b.providerName, b.opts.Provider.Label, "ACP Agent") + " (" + b.key + ")",
		"providerId":   b.providerID,
		"providerName": b.providerName,
		"pid":          nullableInt(pid),
		"cwd":          firstNonEmpty(b.cwd, b.opts.Provider.CWD, b.opts.RootDir),
		"command":      redactSensitiveText(strings.TrimSpace(b.opts.Provider.Command + " " + strings.Join(b.opts.Provider.Args, " "))),
		"chatId":       nullableString(b.chatID),
		"managed":      false,
		"engine":       true,
		"status":       status,
		"code":         nil,
		"startedAt":    timeString(b.startedAt),
		"finishedAt":   nullableTimeString(b.finishedAt),
		"lastLine":     lastLine,
		"state":        string(b.state),
		"rssKb":        b.rssKb,
	}
}

func (b *Bridge) markRecycleAtIdleLocked(reason string) bool {
	if reason == "" {
		reason = "recycle"
	}
	if b.recycleAtIdle && b.recycleReason == reason {
		return false
	}
	b.recycleAtIdle = true
	b.recycleReason = reason
	return true
}

func (b *Bridge) hasRunningJobLocked() bool {
	for _, job := range b.jobsBySession {
		if job != nil && job.Status == "running" {
			return true
		}
	}
	return false
}

func (b *Bridge) hasLiveSession(sessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.sessions[sessionID]
	return ok && !b.closed && b.state != StateHibernated
}

func safeProcessID(raw string) string {
	var out strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "engine"
	}
	return out.String()
}

func processRank(status string) int {
	if status == "running" {
		return 0
	}
	return 1
}

func timeString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func nullableTimeString(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return timeString(t)
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func durationSinceMillis(start, end time.Time) any {
	if start.IsZero() || end.IsZero() {
		return nil
	}
	return end.Sub(start).Milliseconds()
}
