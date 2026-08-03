package main

import (
	"strings"
	"testing"

	"workass/internal/acp"
)

func TestSpawnedWorkNoticeDoesNotSetConversationLanguage(t *testing.T) {
	notice := spawnedWorkServerNoticeText([]acp.SpawnedWorkItem{
		{TaskID: "lane-1", Label: "Build lane", Status: "exited"},
	})

	for _, want := range []string{
		"[workass internal notice]",
		"Background work completed while no turn was active:",
		"This notice is not a user language preference",
		"Resume the work that depended on this completion.",
	} {
		if !strings.Contains(notice, want) {
			t.Fatalf("language-neutral wake marker %q missing from %q", want, notice)
		}
	}
	for _, forbidden := range []string{"Trabajo en segundo plano", "Retoma lo que", "salida:"} {
		if strings.Contains(notice, forbidden) {
			t.Fatalf("wake notice still contains Workass-owned Spanish %q: %q", forbidden, notice)
		}
	}
}
