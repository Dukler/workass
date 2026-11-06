package main

import (
	"strings"
	"testing"
)

func TestAttentionNoticeWordingFollowsWhatHappened(t *testing.T) {
	silent := formatFleetAttention(fleetMonitorNotice{
		TargetID: "job-1", Label: "training run", AttentionKind: "not_reporting",
		AttentionText: "Not reporting: its supervisor (pid 7) is still running but this machine has heard nothing from it for 3m0s",
	})
	if strings.Contains(silent, "another machine") || strings.Contains(silent, "decision") {
		t.Fatalf("a job that stopped reporting is not a decision on another machine: %s", silent)
	}
	if !strings.Contains(silent, "stopped reporting") {
		t.Fatalf("the summary must say the job stopped reporting: %s", silent)
	}
	decision := formatFleetAttention(fleetMonitorNotice{
		TargetID: "agent-2", Label: "remote reviewer", AttentionKind: "permission",
		AttentionText: "Permission required: write a file",
	})
	if !strings.Contains(decision, "needs a decision on another machine") {
		t.Fatalf("a real remote decision keeps its wording: %s", decision)
	}
}
