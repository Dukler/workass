package main

import (
	"sort"
	"strings"

	"workass/internal/chat"
)

// sessionPage is the additive {page} option of session:get, state:digest and
// spawned-work:summary. A renderer that sends it reads only the chats its
// first screen can show, instead of every chat the daemon has ever held
// (2026-10-25: 250 sidebar rows were 297 KB of the 322-485 KB a cold connect
// parsed before its list painted, and 240 of those rows were the hidden,
// searchable archive). t3code does the same: "cold connects skip older
// settled threads" (e57c093c).
//
// A chat is left out only when the renderer's sidebar would file it in the
// archive, which it shows nothing of until a search: it was explicitly settled
// before settledBefore (the renderer picks the instant, with margin), and it
// is read, idle, has nothing queued, waits on no permission, owes nothing
// parked and runs nothing in the background. Everything else is listed, and
// so is every chat the renderer names in include (the chat it shows, a chat a
// notification or an agent asked it to open) and every match of its search.
// The family of a listed chat is listed with it (its parents and all its
// subsessions), because a row nests its subsessions and a subsession names its
// parent. The renderer learns how many chats it was not sent and in which
// folders (deferredChats), which is all its project list and its empty
// archive hint need.
//
// The selection is the same for all three channels, so a digest row exists
// exactly for each listed chat and an unchanged page is still one etag. A
// read without {page} is answered exactly as before.
type sessionPage struct {
	settledBefore int64
	include       map[string]struct{}
	needle        string
	labels        map[string]string
}

// sessionPageFrom reads {page: {settledBefore, include, search, labels}}; nil
// when the option is absent.
func sessionPageFrom(options map[string]any) *sessionPage {
	raw, ok := options["page"].(map[string]any)
	if !ok {
		return nil
	}
	page := &sessionPage{
		settledBefore: int64(max(0, intValue(raw["settledBefore"]))),
		include:       map[string]struct{}{},
		needle:        strings.ToLower(strings.TrimSpace(fieldString(raw, "search"))),
		labels:        map[string]string{},
	}
	for _, item := range anySlice(raw["include"]) {
		if id, ok := item.(string); ok && strings.TrimSpace(id) != "" && len(page.include) < sessionPageIncludeLimit {
			page.include[strings.TrimSpace(id)] = struct{}{}
		}
	}
	for _, item := range anySlice(raw["labels"]) {
		pair := anySlice(item)
		if len(pair) != 2 || len(page.labels) >= sessionPageIncludeLimit {
			continue
		}
		cwd, okCWD := pair[0].(string)
		label, okLabel := pair[1].(string)
		if okCWD && okLabel {
			page.labels[cwd] = label
		}
	}
	return page
}

const sessionPageIncludeLimit = 4096

type sessionPageSelection struct {
	chats        map[string]struct{}
	deferred     int
	deferredCWDs map[string]int
}

func (s *sessionPageSelection) has(chatID string) bool {
	if s == nil {
		return true
	}
	_, ok := s.chats[chatID]
	return ok
}

// deferredProjection is the reply's deferredChats: {count, cwds: [[cwd, n]]}.
func (s *sessionPageSelection) deferredProjection() map[string]any {
	cwds := make([]string, 0, len(s.deferredCWDs))
	for cwd := range s.deferredCWDs {
		cwds = append(cwds, cwd)
	}
	sort.Strings(cwds)
	pairs := make([]any, 0, len(cwds))
	for _, cwd := range cwds {
		pairs = append(pairs, []any{cwd, s.deferredCWDs[cwd]})
	}
	return map[string]any{"count": s.deferred, "cwds": pairs}
}

func (p *sessionPage) lists(facts chat.PageFacts) bool {
	if _, named := p.include[facts.TabID]; named {
		return true
	}
	if facts.Settled != "settled" || facts.Unread || facts.Foreground || facts.PendingPermissions ||
		facts.Queued || facts.RunningBackground || facts.ObligationState == "parked" {
		return true
	}
	// The renderer's settledSince: the explicit settle instant, else the
	// last activity, else "always" (a row predating the timestamp contract).
	since := facts.SettledAt
	if since <= 0 {
		since = facts.LastActivityAt
	}
	if since <= 0 {
		since = 1
	}
	if since >= p.settledBefore {
		return true
	}
	return p.matches(facts)
}

// matches is a superset of the renderer's own search, which it applies again
// to whatever it receives: `${title} ${project}` contains the needle.
func (p *sessionPage) matches(facts chat.PageFacts) bool {
	if p.needle == "" {
		return false
	}
	title := strings.ToLower(facts.Title)
	if strings.Contains(title, p.needle) {
		return true
	}
	for _, label := range []string{p.labels[facts.CWD], sessionPageWorkspaceName(facts.CWD)} {
		if label != "" && strings.Contains(title+" "+strings.ToLower(label), p.needle) {
			return true
		}
	}
	return false
}

// sessionPageWorkspaceName is the renderer's workspaceName(path).
func sessionPageWorkspaceName(path string) string {
	clean := strings.TrimSpace(path)
	for len(clean) > 1 && (strings.HasSuffix(clean, "/") || strings.HasSuffix(clean, `\`)) && !(len(clean) == 3 && clean[1] == ':') {
		clean = clean[:len(clean)-1]
	}
	if clean == "" {
		return "Chats"
	}
	parts := strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return clean
	}
	return parts[len(parts)-1]
}

// A paged shell's chatOrder lists only what the shell lists, plus every other
// machine's id (the renderer anchors those rows itself), and says so with
// chatOrderPartial: the whole order was 11 KB of a 31 KB first page. A renderer
// that holds such an order saves it with chatOrderPartial too, and the daemon
// folds it into the stored order (mergePartialChatOrder) instead of replacing
// it, so every chat it was never sent keeps its place.

// partialChatOrder is the stored order restricted to the listed tabs and to
// other machines' ids.
func partialChatOrder(raw any, listed map[string]struct{}) []any {
	out := []any{}
	for _, value := range normalizedActorChatOrder(raw) {
		id, _ := value.(string)
		if _, ok := listed[id]; ok || strings.HasPrefix(id, machineTaggedIDPrefix) {
			out = append(out, id)
		}
	}
	return out
}

// mergePartialChatOrder folds a renderer's partial order into the stored one
// the way the renderer folds its visible rows into its own order
// (mergeVisibleChatOrder): an id the renderer does not name keeps its slot,
// the ids it names fill their own slots in its order, and an id new to the
// stored order goes first (a new chat is the one automatic movement). An id of
// this daemon that is no longer a chat is dropped, as a renderer holding the
// whole order would have dropped it. Folding the same order twice changes
// nothing, so a retried save names the same content.
func mergePartialChatOrder(stored, incoming []any, live map[string]struct{}) []any {
	named := make(map[string]struct{}, len(incoming))
	for _, value := range incoming {
		id, _ := value.(string)
		named[id] = struct{}{}
	}
	present := make(map[string]struct{}, len(stored))
	for _, value := range stored {
		id, _ := value.(string)
		present[id] = struct{}{}
	}
	added := make([]any, 0)
	reordered := make([]any, 0, len(incoming))
	for _, value := range incoming {
		id, _ := value.(string)
		if _, ok := present[id]; ok {
			reordered = append(reordered, id)
		} else {
			added = append(added, id)
		}
	}
	merged := append([]any{}, added...)
	cursor := 0
	for _, value := range stored {
		id, _ := value.(string)
		if _, ok := named[id]; ok {
			merged = append(merged, reordered[cursor])
			cursor++
			continue
		}
		if _, ok := live[id]; ok || strings.HasPrefix(id, machineTaggedIDPrefix) {
			merged = append(merged, id)
		}
	}
	merged = append(merged, reordered[cursor:]...)
	return normalizedActorChatOrder(merged)
}

// liveTabIDs are the tab ids of this daemon's chats that are not deleted.
func (r *providerChatRuntime) liveTabIDs() (map[string]struct{}, error) {
	ids, err := r.knownChatIDs()
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(ids))
	for _, chatID := range ids {
		facts, err := r.pageFacts(chatID)
		if err != nil {
			return nil, err
		}
		if !facts.Deleted && facts.TabID != "" {
			out[facts.TabID] = struct{}{}
		}
	}
	return out, nil
}

// pageFacts reads one known chat's facts without opening a closed actor.
func (r *providerChatRuntime) pageFacts(chatID string) (chat.PageFacts, error) {
	actor, summary, err := r.residentOrClosed(chatID)
	if err != nil {
		return chat.PageFacts{}, err
	}
	if summary != nil {
		return summary.PageFacts(), nil
	}
	return actor.engine.PageFacts(), nil
}

// selectSessionPage decides which chats a {page} read lists. activeTabID is
// the daemon's own selection, which is always listed.
func (r *providerChatRuntime) selectSessionPage(page *sessionPage, activeTabID string) (*sessionPageSelection, error) {
	if page == nil {
		return nil, nil
	}
	ids, err := r.knownChatIDs()
	if err != nil {
		return nil, err
	}
	facts := make([]chat.PageFacts, 0, len(ids))
	tabByChat := make(map[string]string, len(ids))
	for _, chatID := range ids {
		item, err := r.pageFacts(chatID)
		if err != nil {
			return nil, err
		}
		if item.Deleted {
			continue
		}
		facts = append(facts, item)
		tabByChat[item.ChatID] = item.TabID
	}
	parentOf := func(item chat.PageFacts) string {
		if parent := strings.TrimSpace(item.ParentTabID); parent != "" {
			return parent
		}
		return tabByChat[strings.TrimSpace(item.ParentChatID)]
	}
	// A running fork child makes its parent row working, like its own turn.
	var forking map[string]struct{}
	if r.forkRunning != nil {
		forking = r.forkRunning()
	}
	listed := make(map[string]struct{}, len(facts))
	children := make(map[string][]string)
	parents := make(map[string]string, len(facts))
	for _, item := range facts {
		if parent := parentOf(item); parent != "" {
			parents[item.TabID] = parent
			children[parent] = append(children[parent], item.TabID)
		}
		_, forkLive := forking[item.ChatID]
		if item.TabID == activeTabID || forkLive || page.lists(item) {
			listed[item.TabID] = struct{}{}
		}
	}
	// A listed chat brings its whole family: every ancestor, then every
	// descendant of everything listed.
	queue := make([]string, 0, len(listed))
	for tab := range listed {
		queue = append(queue, tab)
	}
	for _, tab := range queue {
		for parent, seen := parents[tab], 0; parent != "" && seen < len(facts); parent, seen = parents[parent], seen+1 {
			if _, ok := listed[parent]; ok {
				break
			}
			listed[parent] = struct{}{}
		}
	}
	queue = queue[:0]
	for tab := range listed {
		queue = append(queue, tab)
	}
	for len(queue) > 0 {
		tab := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, child := range children[tab] {
			if _, ok := listed[child]; ok {
				continue
			}
			listed[child] = struct{}{}
			queue = append(queue, child)
		}
	}
	selection := &sessionPageSelection{chats: make(map[string]struct{}, len(listed)), deferredCWDs: map[string]int{}}
	for _, item := range facts {
		if _, ok := listed[item.TabID]; ok {
			selection.chats[item.ChatID] = struct{}{}
			continue
		}
		selection.deferred++
		selection.deferredCWDs[item.CWD]++
	}
	return selection, nil
}
