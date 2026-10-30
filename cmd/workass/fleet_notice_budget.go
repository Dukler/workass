package main

import (
	"fmt"
	"sync"
	"time"

	"workass/internal/fleetlog"
)

// The notice budget: what is left of §16.3b once a child's end stopped being
// capped (§14h).
//
// Coalescing collapses the SAME event arriving more than once inside the 60 s
// window: a push that raced a reconnect's backlog, a redelivery after a failed
// ack. It does not merge two DIFFERENT children's results into one line. A
// result is content; dropping one to save a line in the transcript would lose
// the thing the notice exists to carry.
//
// There is no hourly cap. It held only successful ends, since failures,
// attentions and home_down were all exempt, and what it held was gone: the
// monitor's cursor had moved, its home had been acknowledged, and a local child
// was never capped at all. Found live: a chat's seventh child in an hour settled
// its row and told nobody. A chat receives one end per child it started, so the
// bound on these notices is the work it asked for.
const fleetNoticeWindow = time.Minute

type fleetNoticeBudget struct {
	window time.Duration

	mu     sync.Mutex
	recent map[string][]fleetNoticeRecord
}

type fleetNoticeRecord struct {
	key string
	at  time.Time
}

func newFleetNoticeBudget() *fleetNoticeBudget {
	return &fleetNoticeBudget{window: fleetNoticeWindow, recent: map[string][]fleetNoticeRecord{}}
}

// admit decides whether one notice reaches its chat. The second result names
// why it did not, so a held notice is a log line with a reason rather than
// silence.
func (b *fleetNoticeBudget) admit(notice fleetMonitorNotice, now time.Time) (bool, string) {
	if b == nil {
		return true, ""
	}
	chat := notice.TabID + "\x00" + notice.ChatID
	key := noticeIdentity(notice)
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := make([]fleetNoticeRecord, 0, len(b.recent[chat])+1)
	duplicate := false
	for _, record := range b.recent[chat] {
		if now.Sub(record.at) > b.window {
			continue
		}
		duplicate = duplicate || record.key == key
		kept = append(kept, record)
	}
	if duplicate {
		b.recent[chat] = kept
		return false, fmt.Sprintf("the same %s notice already reached this chat inside the %s window", notice.Kind, b.window)
	}
	b.recent[chat] = append(kept, fleetNoticeRecord{key: key, at: now})
	return true, ""
}

// noticeIdentity is what makes two arrivals the same notice: the chat is
// already the bucket, so identity is the event — its kind, its entity, and the
// durable receipt or home the event is about.
func noticeIdentity(notice fleetMonitorNotice) string {
	switch notice.Kind {
	case fleetMonitorHomeDown, fleetMonitorHomeUp:
		return notice.Kind + "\x00" + notice.Home
	case fleetlog.KindAttention:
		return notice.Kind + "\x00" + notice.TargetID + "\x00" + notice.AttentionText
	default:
		return notice.Kind + "\x00" + notice.TargetID + "\x00" + notice.Receipt.ReceiptID
	}
}
