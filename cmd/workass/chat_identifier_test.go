package main

import (
	"strings"
	"testing"
)

// 2026-10-11 QA: a 300-character chat id was accepted, and the daemon later
// could not boot because its spawned-work cache file name was too long.
func TestChatIdentifiersAreBoundedAndFileSafe(t *testing.T) {
	for _, id := range []string{"tab-d98234b438ef3777546e9bc0", "conv-mupvnnec-8pokfn", "tab-1790879906100-85", "chat-81716b9c2a9a4a9a030631af"} {
		if err := validateChatIdentifier(id); err != nil {
			t.Fatalf("real id %q refused: %v", id, err)
		}
	}
	for _, id := range []string{strings.Repeat("x", 300), "../x", "a b", "tab/../../etc"} {
		if err := validateChatIdentifier(id); err == nil {
			t.Fatalf("unsafe chat id %q accepted", id)
		}
	}
}
