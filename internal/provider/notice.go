package provider

import "strings"

// NoticeView is the renderer's quiet-line view of a Workass wake-up notice
// (a tracked subagent finished): what finished, how, and its result. Nil when
// kind is empty. Every surface that shows a notice turn's user row — the live
// job, the pending and settled rows, the queue — uses this one shape, so the
// row never falls back to a bubble as if the user had written it (user
// 2026-10-16).
func NoticeView(kind, title, status, text string) map[string]any {
	if strings.TrimSpace(kind) == "" {
		return nil
	}
	view := map[string]any{"kind": kind, "title": title, "status": status}
	if body := NoticeBody(text); body != "" {
		view["body"] = body
	}
	return view
}

// NoticeBody extracts a notice's result: the <result>/<error> of a
// task-notification, or the Result:/Failure: section of the legacy text.
func NoticeBody(text string) string {
	for _, tag := range []string{"result", "error"} {
		open, close := "<"+tag+">", "</"+tag+">"
		if start := strings.Index(text, open); start >= 0 {
			if end := strings.LastIndex(text, close); end > start {
				return strings.TrimSpace(text[start+len(open) : end])
			}
		}
	}
	for _, marker := range []string{"\nResult:\n", "\nFailure:\n"} {
		if index := strings.Index(text, marker); index >= 0 {
			return strings.TrimSpace(text[index+len(marker):])
		}
	}
	return ""
}
