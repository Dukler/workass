package main

import (
	"fmt"
	"sync"
	"time"

	"workass/internal/fleetlog"
)

// The notice budget: §16.3b's two decided numbers, applied to the monitor
// projection.
//
// "Notices are coalesced per chat per 60 s, at most 6 per chat per hour, and
// failures are always exempt from the cap" (docs/specs/fleet-runtime.md §7,
// §16.3b). Both numbers are fixed rather than configurable, for the same reason
// the ten-minute threshold is: the operator decided three numbers, and a knob
// beside a decided number is how a decided number stops being one.
//
// Two deliberate readings, because the difference matters and a reader of this
// file should not have to guess which one it got:
//
//   - Coalescing here collapses the SAME event arriving more than once inside
//     the window — a push that raced a reconnect's backlog, a redelivery after
//     a failed ack. It does not merge two DIFFERENT children's results into one
//     line. A result is content; dropping one to save a line in the transcript
//     would lose the thing the notice exists to carry. Folding several distinct
//     settles into one aggregate line is `delegation-fabric.md` P4's change and
//     is gated by the projection-parity test, so it is not made here.
//   - The cap drops; it does not queue. A queued notice needs a timer to flush
//     it, and a notice that arrives an hour late is worse than a notice that
//     never arrived and said so in the log — the work is still readable at its
//     home, which is the whole point of events being log entries rather than
//     assertions.
//
// Failures, attentions and `home_down` are exempt. A storm guard that silenced
// "this failed", "something is waiting on you" or "I cannot see that machine"
// would hide exactly the events a person needs, which is the opposite of what a
// cap is for.
const (
	fleetNoticeWindow  = time.Minute
	fleetNoticeCapHour = 6
	fleetNoticeCapSpan = time.Hour
)

type fleetNoticeBudget struct {
	window time.Duration
	cap    int
	span   time.Duration

	mu     sync.Mutex
	recent map[string][]fleetNoticeRecord
}

type fleetNoticeRecord struct {
	key     string
	at      time.Time
	counted bool
}

func newFleetNoticeBudget() *fleetNoticeBudget {
	return &fleetNoticeBudget{
		window: fleetNoticeWindow, cap: fleetNoticeCapHour, span: fleetNoticeCapSpan,
		recent: map[string][]fleetNoticeRecord{},
	}
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
	counted := 0
	for _, record := range b.recent[chat] {
		if now.Sub(record.at) > b.span {
			continue
		}
		if record.key == key && now.Sub(record.at) <= b.window {
			b.recent[chat] = append(kept, b.recent[chat][len(kept):]...)
			return false, fmt.Sprintf("the same %s notice already reached this chat inside the %s window", notice.Kind, b.window)
		}
		kept = append(kept, record)
		if record.counted {
			counted++
		}
	}
	exempt := notice.Failure
	if !exempt && counted >= b.cap {
		b.recent[chat] = kept
		return false, fmt.Sprintf("this chat is at its %d-notice hourly cap; the work is still readable at its home", b.cap)
	}
	b.recent[chat] = append(kept, fleetNoticeRecord{key: key, at: now, counted: !exempt})
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
