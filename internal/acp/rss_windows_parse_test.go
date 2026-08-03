package acp

import "testing"

func TestParseTasklistRSSKB(t *testing.T) {
	fixture := `"Image Name","PID","Session Name","Session#","Mem Usage"
"node.exe","42","Console","1","123,456 K"
"workass.exe","77","Services","0","9,876 K"
`
	got, err := parseTasklistRSSKB(fixture, 42)
	if err != nil {
		t.Fatalf("parse tasklist: %v", err)
	}
	if got != 123456 {
		t.Fatalf("rssKb = %d, want 123456", got)
	}
}

func TestParseTasklistRSSKBNoMatch(t *testing.T) {
	if _, err := parseTasklistRSSKB(`INFO: No tasks are running which match the specified criteria.`, 42); err == nil {
		t.Fatal("expected no-match error")
	}
}
