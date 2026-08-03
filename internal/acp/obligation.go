package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// An obligation is what the chat owes the user: it opens when a human asks for
// something and closes when that thing is answered. It is deliberately NOT a
// per-turn record — one request can span many turns as an agent parks and is
// woken — and deliberately not a per-prompt ledger either, because prompts
// serialize per bridge (manager.go:3250) so a chat can only ever owe one thing
// at a time.
const (
	obligationWorking    = "working"
	obligationParked     = "parked"
	obligationNeedsInput = "needs_input"
	obligationDone       = "done"
	obligationStalled    = "stalled"
)

const (
	obligationCloseSuperseded = "superseded"
	obligationCloseCancelled  = "cancelled"
)

// stalledGrace is deliberately generous: a self-scheduled wake is invisible to
// the daemon and clamps at an hour in the harness, so 90 minutes is 1.5x the
// worst legitimate case. A shorter clock would turn ordinary parking into a
// false alarm, which is the failure that teaches the user to ignore the cue.
const stalledGrace = 90 * time.Minute

const maxObligationReceipts = 128

type ObligationRecord struct {
	TabID       string `json:"tabId"`
	ChatID      string `json:"chatId"`
	State       string `json:"state"`
	Source      string `json:"source,omitempty"`
	CloseReason string `json:"closeReason,omitempty"`
	Note        string `json:"note,omitempty"`
	OpenedAt    string `json:"openedAt"`
	UpdatedAt   string `json:"updatedAt"`
	ClosedAt    string `json:"closedAt,omitempty"`
	PromptID    string `json:"promptId,omitempty"`
	ParkedSince string `json:"parkedSince,omitempty"`
}

type obligationSnapshot struct {
	Open   []ObligationRecord `json:"open"`
	Closed []ObligationRecord `json:"closed,omitempty"`
}

func obligationKey(tabID, chatID string) string {
	return strings.TrimSpace(tabID) + "\x00" + strings.TrimSpace(chatID)
}

func (m *Manager) obligationSnapshotPath(tabID string) string {
	tabID = strings.TrimSpace(tabID)
	if tabID == "" || strings.TrimSpace(m.opts.StateDir) == "" || strings.ContainsAny(tabID, `/\`) {
		return ""
	}
	return filepath.Join(m.opts.StateDir, "obligations", tabID+".json")
}

// openObligation is called only for a human-authored prompt. Any obligation
// already open is closed as superseded rather than overwritten: the user
// changing their mind is a fact worth keeping, and silently mutating the old
// record would lose which prompt the earlier work belonged to.
func (m *Manager) openObligation(tabID, chatID, promptID string) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return
	}
	now := isoNow()
	m.obligationMu.Lock()
	key := obligationKey(tabID, chatID)
	// A declaration that outlived its turn belongs to the request being
	// superseded, not to the one being asked now. Left in place it would be
	// clamped onto the next turn's outcome — a verdict about work the user has
	// already moved on from.
	if prior := m.obligations[key]; prior != nil {
		m.closeObligationLocked(prior, obligationCloseSuperseded, now)
	}
	m.obligations[key] = &ObligationRecord{
		TabID: tabID, ChatID: chatID, State: obligationWorking,
		OpenedAt: now, UpdatedAt: now, PromptID: strings.TrimSpace(promptID),
	}
	m.obligationMu.Unlock()
	m.persistObligations(tabID)
}

// resumeObligation is called when a turn starts WITHOUT a new human prompt — a
// wake delivering settled work, or any other self-resumption. It reopens the
// obligation the chat already had, which is what retroactively rescinds a
// close: a chat that resumes by itself was never finished, whatever the
// previous turn's end looked like.
func (m *Manager) resumeObligation(tabID, chatID string) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return
	}
	now := isoNow()
	m.obligationMu.Lock()
	key := obligationKey(tabID, chatID)
	rec := m.obligations[key]
	if rec == nil {
		// The close already happened, so reclaim the newest receipt rather
		// than inventing a fresh obligation with no history.
		rec = m.reclaimClosedObligationLocked(key, tabID, chatID, now)
	}
	rec.State = obligationWorking
	rec.CloseReason = ""
	rec.ClosedAt = ""
	rec.ParkedSince = ""
	rec.UpdatedAt = now
	m.obligations[key] = rec
	m.obligationMu.Unlock()
	m.persistObligations(tabID)
}

func (m *Manager) reclaimClosedObligationLocked(key, tabID, chatID, now string) *ObligationRecord {
	list := m.obligationReceipts[key]
	if len(list) > 0 {
		revived := list[len(list)-1]
		m.obligationReceipts[key] = list[:len(list)-1]
		return &revived
	}
	return &ObligationRecord{TabID: tabID, ChatID: chatID, OpenedAt: now, PromptID: ""}
}

// settleObligation applies one finished turn. hasLiveEvidence is supplied by
// the caller rather than computed here because collecting it costs a process
// probe; see the clamp in obligation_clamp.go.
func (m *Manager) settleObligation(tabID, chatID string, signal CompletionSignal, hasLiveEvidence bool) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" || signal.Disposition == DispositionDeferred {
		// Deferred means the daemon itself ended the turn while shutting down.
		// The record stays exactly as it is and the boot sweep settles it,
		// because a verdict written during teardown may never reach disk.
		return
	}
	now := isoNow()
	m.obligationMu.Lock()
	key := obligationKey(tabID, chatID)
	rec := m.obligations[key]
	if rec == nil {
		m.obligationMu.Unlock()
		return
	}
	switch signal.Disposition {
	case DispositionCancelled:
		m.closeObligationLocked(rec, obligationCloseCancelled, now)
		delete(m.obligations, key)
		m.obligationMu.Unlock()
		m.persistObligations(tabID)
		return
	case DispositionParked:
		rec.State = obligationParked
		rec.ParkedSince = now
	case DispositionNeedsInput:
		rec.State = obligationNeedsInput
		rec.ParkedSince = ""
	case DispositionDone:
		rec.State = obligationDone
		rec.ParkedSince = ""
	default:
		// Unknown. The turn yielded and said nothing either way, so the
		// evidence decides: something alive to resume this means parked,
		// nothing alive means the request is as answered as it will get.
		if hasLiveEvidence {
			rec.State = obligationParked
			rec.ParkedSince = now
			signal.Source = dispositionSourceInferred
		} else {
			rec.State = obligationDone
			signal.Source = dispositionSourceInferred
		}
	}
	// A declared or native done is demoted by live evidence, never promoted by
	// its absence: the model may know something the daemon cannot see, but it
	// cannot un-arm a wake that is genuinely pending.
	if rec.State == obligationDone && hasLiveEvidence {
		rec.State = obligationParked
		rec.ParkedSince = now
		signal.Source = dispositionSourceInferred
	}
	rec.Source = signal.Source
	if note := compactText(signal.Note, maxDispositionNote); note != "" {
		rec.Note = note
	}
	rec.UpdatedAt = now
	m.obligationMu.Unlock()
	m.persistObligations(tabID)
}

func (m *Manager) closeObligationLocked(rec *ObligationRecord, reason, now string) {
	if rec == nil {
		return
	}
	closed := *rec
	closed.CloseReason = reason
	closed.ClosedAt = now
	closed.UpdatedAt = now
	key := obligationKey(rec.TabID, rec.ChatID)
	list := append(m.obligationReceipts[key], closed)
	if len(list) > maxObligationReceipts {
		list = list[len(list)-maxObligationReceipts:]
	}
	m.obligationReceipts[key] = list
}

// markObligationStalled is the one transition the user is meant to act on that
// nobody declared: a chat that claims to be waiting on something which is not
// there. It is reached from the periodic sweep and from boot.
func (m *Manager) markObligationStalled(rec *ObligationRecord, now string) bool {
	if rec == nil || rec.State == obligationStalled {
		return false
	}
	rec.State = obligationStalled
	rec.Source = dispositionSourceInferred
	rec.ParkedSince = ""
	rec.UpdatedAt = now
	return true
}

// ObligationFor is the read surface. It returns nil when the chat owes
// nothing, which a caller must not confuse with "done".
func (m *Manager) ObligationFor(tabID, chatID string) map[string]any {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return nil
	}
	m.obligationMu.Lock()
	defer m.obligationMu.Unlock()
	rec := m.obligations[obligationKey(tabID, chatID)]
	if rec == nil {
		return nil
	}
	out := map[string]any{"state": rec.State, "openedAt": rec.OpenedAt, "updatedAt": rec.UpdatedAt}
	if rec.Source != "" {
		out["source"] = rec.Source
	}
	if rec.Note != "" {
		out["note"] = redactSensitiveText(rec.Note)
	}
	if rec.PromptID != "" {
		out["promptId"] = rec.PromptID
	}
	return out
}

func (m *Manager) persistObligations(tabID string) {
	path := m.obligationSnapshotPath(tabID)
	if path == "" {
		return
	}
	snapshot := obligationSnapshot{}
	m.obligationMu.Lock()
	for _, rec := range m.obligations {
		if rec != nil && rec.TabID == tabID {
			snapshot.Open = append(snapshot.Open, *rec)
		}
	}
	for _, list := range m.obligationReceipts {
		for _, rec := range list {
			if rec.TabID == tabID {
				snapshot.Closed = append(snapshot.Closed, rec)
			}
		}
	}
	m.obligationMu.Unlock()
	sort.SliceStable(snapshot.Open, func(i, j int) bool { return snapshot.Open[i].ChatID < snapshot.Open[j].ChatID })
	sort.SliceStable(snapshot.Closed, func(i, j int) bool { return snapshot.Closed[i].ClosedAt < snapshot.Closed[j].ClosedAt })
	data, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		if os.Rename(tmp, path) != nil {
			_ = os.Remove(tmp)
		}
	}
}

// DropObligationsForChat removes a deleted chat's records. Keeping them would
// leave the daemon asserting that a chat which no longer exists still owes the
// user an answer.
func (m *Manager) DropObligationsForChat(tabID, chatID string) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return
	}
	key := obligationKey(tabID, chatID)
	m.obligationMu.Lock()
	delete(m.obligations, key)
	delete(m.obligationReceipts, key)
	m.obligationMu.Unlock()
	m.persistObligations(tabID)
}

func (m *Manager) loadObligationSnapshots() {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return
	}
	paths, _ := filepath.Glob(filepath.Join(m.opts.StateDir, "obligations", "*.json"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var snapshot obligationSnapshot
		if json.Unmarshal(data, &snapshot) != nil {
			continue
		}
		for _, rec := range snapshot.Open {
			if rec.TabID == "" || rec.ChatID == "" {
				continue
			}
			stored := rec
			m.obligations[obligationKey(rec.TabID, rec.ChatID)] = &stored
		}
		for _, rec := range snapshot.Closed {
			if rec.TabID == "" || rec.ChatID == "" {
				continue
			}
			key := obligationKey(rec.TabID, rec.ChatID)
			m.obligationReceipts[key] = append(m.obligationReceipts[key], rec)
		}
	}
}
