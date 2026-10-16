package provider

import "testing"

func TestNoticeViewShapesEveryNoticeAlike(t *testing.T) {
	if NoticeView("", "x", "done", "y") != nil {
		t.Fatal("a turn without a notice kind produced a notice")
	}
	view := NoticeView("subagent", "Revisión", "done", "<task-notification>\n<result>todo bien</result>\n</task-notification>")
	if view["kind"] != "subagent" || view["title"] != "Revisión" || view["status"] != "done" || view["body"] != "todo bien" {
		t.Fatalf("view = %#v", view)
	}
	if got := NoticeBody("<task-notification><error>boom</error></task-notification>"); got != "boom" {
		t.Fatalf("error body = %q", got)
	}
	if got := NoticeBody("Internal agent completion notice.\nAgent: a\nResult:\nlegacy text"); got != "legacy text" {
		t.Fatalf("legacy body = %q", got)
	}
}
