package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/procchild"
	"workass/internal/wire"
)

func TestJobSettingsSurviveTheWholeBlobReplace(t *testing.T) {
	settings := normalizeSettings(map[string]any{"jobs": map[string]any{"silenceNoticeAfterMin": json.Number("30")}})
	if got := mapFromAnyMain(settings["jobs"])["silenceNoticeAfterMin"]; got != 30 {
		t.Fatalf("a stored choice must survive settings:set, got %#v", settings["jobs"])
	}
	for _, bad := range []any{-5, 0, "soon", 24*60 + 1, nil} {
		settings := normalizeSettings(map[string]any{"jobs": map[string]any{"silenceNoticeAfterMin": bad}})
		if got := mapFromAnyMain(settings["jobs"])["silenceNoticeAfterMin"]; got != 0 {
			t.Fatalf("%#v must read as the default, got %#v", bad, got)
		}
	}
	if got := mapFromAnyMain(normalizeSettings(nil)["jobs"])["silenceNoticeAfterMin"]; got != 0 {
		t.Fatalf("a blob without the key reads the default, got %#v", got)
	}
}

func TestSettingsSetHandsTheSilenceNoticeDelayToTheManager(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	// A choice already stored is applied at boot, before anybody saves.
	if err := os.WriteFile(filepath.Join(stateDir, "app-settings.json"), []byte(`{"jobs":{"silenceNoticeAfterMin":10}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	hub := wire.NewHub()
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})
	if got := manager.ProcessSilenceNoticeAfter(); got != 10*time.Minute {
		t.Fatalf("boot delay = %s, want 10m", got)
	}

	reply, err := hub.Invoke("settings:get", nil)
	if err != nil {
		t.Fatal(err)
	}
	blob := mapFromAnyMain(mapFromAnyMain(reply)["settings"])
	blob["jobs"] = map[string]any{"silenceNoticeAfterMin": 60}
	if _, err := hub.Invoke("settings:set", []any{blob}); err != nil {
		t.Fatal(err)
	}
	if got := manager.ProcessSilenceNoticeAfter(); got != time.Hour {
		t.Fatalf("delay after settings:set = %s, want 1h", got)
	}
	// Another writer's whole blob without the key puts the default back.
	delete(blob, "jobs")
	if _, err := hub.Invoke("settings:set", []any{blob}); err != nil {
		t.Fatal(err)
	}
	if got := manager.ProcessSilenceNoticeAfter(); got != 0 {
		t.Fatalf("delay after a blob without jobs = %s, want 0", got)
	}
}

func TestResourcesListNamesTheHolderTheQueueAndTheFreeOnes(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	hub := wire.NewHub()
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	// gpu0: this process holds it, and two more claims wait behind it.
	lease, err := procchild.Acquire(procchild.AcquireRequest{
		StateDir: stateDir, Resource: "gpu0", ChildID: "xwHOLDER01", Label: "train cf64", PID: os.Getpid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	queue := procchild.ResourceQueueDir(stateDir, "gpu0")
	for i, child := range []string{"xwWAITER01", "xwWAITER02"} {
		ticket := fmt.Sprintf("%020d", time.Now().Add(time.Duration(i+1)*time.Second).UnixNano())
		claim, _ := json.Marshal(map[string]any{
			"ticket": ticket, "pid": os.Getpid(), "childId": child, "label": "wait", "since": time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err := os.WriteFile(filepath.Join(queue, "q-"+ticket+"-"+child+".claim"), claim, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// cpu-big: used once, nobody on it now.
	if err := os.MkdirAll(procchild.ResourceQueueDir(stateDir, "cpu-big"), 0o700); err != nil {
		t.Fatal(err)
	}

	reply, err := hub.Invoke("resources:list", nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(reply)
	var got struct {
		Resources []struct {
			Name    string `json:"name"`
			Waiting int    `json:"waiting"`
			Holder  *struct {
				WorkID    string `json:"workId"`
				Label     string `json:"label"`
				HeldSince string `json:"heldSince"`
			} `json:"holder"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Resources) != 2 || got.Resources[0].Name != "cpu-big" || got.Resources[1].Name != "gpu0" {
		t.Fatalf("resources = %s", encoded)
	}
	if free := got.Resources[0]; free.Holder != nil || free.Waiting != 0 {
		t.Fatalf("a resource nobody holds is free: %s", encoded)
	}
	busy := got.Resources[1]
	if busy.Holder == nil || busy.Holder.WorkID != "xwHOLDER01" || busy.Holder.Label != "train cf64" || busy.Holder.HeldSince == "" {
		t.Fatalf("holder = %s", encoded)
	}
	if busy.Waiting != 2 {
		t.Fatalf("waiting = %d, want 2: %s", busy.Waiting, encoded)
	}
}
