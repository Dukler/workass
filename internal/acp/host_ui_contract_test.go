package acp

import (
	"strings"
	"testing"
)

func TestWorkassPromptsDoNotRestrictHostUIOrExternalBrowsers(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{WorkassToolsOrigin: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })

	for _, prompt := range []string{
		manager.buildAppChatPrompt(JobStartOptions{HumanAuthored: true}, "inspect the current Workass state"),
		manager.buildUserRequestBlock("continue inspecting the same Workass state", true),
		manager.buildEnvironmentBrief(true),
	} {
		for _, removed := range []string{
			"Host UI rule:", "Do not open OS windows", "never use OS accessibility",
			"instead of another browser process", "report the limitation instead",
			"do not ask the user to remind you",
		} {
			if strings.Contains(prompt, removed) {
				t.Fatalf("prompt still includes removed UI/browser restriction %q", removed)
			}
		}
	}
}
