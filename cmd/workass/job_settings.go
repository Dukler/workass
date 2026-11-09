package main

import (
	"sort"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/procchild"
	"workass/internal/wire"
)

// Ajustes · Trabajos (Settings rework, 2026-11-09): the long jobs agents start
// with `workass run`. Two things on that page are this daemon's:
//
//   - "jobs".silenceNoticeAfterMin in the app-settings blob: how long a running
//     job's supervisor may go unheard before its chat is TOLD. Zero is the
//     engine's own rule, the notice with the mark (three missed beats, longer
//     after a restart). It never moves the mark itself, so a job's row still
//     reads silent at the window (acp.Manager.SetProcessSilenceNoticeAfter). It
//     is this machine's setting: each daemon watches the jobs it runs.
//   - resources:list, a read: every resource a job on this machine has queued
//     for, who holds it and how many wait. A resource is held by one job at a
//     time (procchild.Acquire), so there is no concurrency to configure.

const (
	jobSettingsKey              = "jobs"
	jobSilenceNoticeAfterMinKey = "silenceNoticeAfterMin"
)

func defaultJobSettings() map[string]any {
	return map[string]any{jobSilenceNoticeAfterMinKey: 0}
}

// normalizeJobSettings keeps "jobs" through settings:set, which replaces the
// whole blob: whole minutes, zero for the default, at most a day.
func normalizeJobSettings(value any) map[string]any {
	out := defaultJobSettings()
	minutes, ok := intFieldOK(mapFromAnyMain(value), jobSilenceNoticeAfterMinKey)
	if ok && minutes > 0 && time.Duration(minutes)*time.Minute <= acp.MaxProcessSilenceNoticeAfter {
		out[jobSilenceNoticeAfterMinKey] = minutes
	}
	return out
}

// applyJobSettings hands the stored choice to the manager, at boot and after
// every settings:set.
func applyJobSettings(manager *acp.Manager, settings map[string]any) {
	if manager == nil {
		return
	}
	minutes, _ := intFieldOK(normalizeJobSettings(settings[jobSettingsKey]), jobSilenceNoticeAfterMinKey)
	manager.SetProcessSilenceNoticeAfter(time.Duration(minutes) * time.Minute)
}

// resourceHolderView is the job holding a resource.
type resourceHolderView struct {
	WorkID string `json:"workId"`
	Label  string `json:"label,omitempty"`
	// HeldSince and HeldForMs are absent until the holder's supervisor stated
	// when it took the hold: time in the queue is never hold time.
	HeldSince string `json:"heldSince,omitempty"`
	HeldForMs int64  `json:"heldForMs,omitempty"`
}

// resourceView is one resource on this machine.
type resourceView struct {
	Name    string              `json:"name"`
	Holder  *resourceHolderView `json:"holder,omitempty"`
	Waiting int                 `json:"waiting"`
}

// resourceOverview folds the visible queues into one row per resource. A
// resource with no claim is free; the first claim holds it and the rest wait.
func resourceOverview(names []string, entries []procchild.QueueEntry) []resourceView {
	byName := map[string]*resourceView{}
	add := func(name string) *resourceView {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil
		}
		if row := byName[name]; row != nil {
			return row
		}
		row := &resourceView{Name: name}
		byName[name] = row
		return row
	}
	for _, name := range names {
		add(name)
	}
	for _, entry := range entries {
		row := add(entry.Resource)
		if row == nil {
			continue
		}
		if entry.Holding && row.Holder == nil {
			row.Holder = &resourceHolderView{
				WorkID: entry.ChildID, Label: acp.RedactSensitiveText(strings.TrimSpace(entry.Label)),
				HeldSince: entry.HeldSince, HeldForMs: entry.HeldForMs,
			}
			continue
		}
		row.Waiting++
	}
	out := make([]resourceView, 0, len(byName))
	for _, row := range byName {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// registerJobResourceHandler is resources:list [] → {resources}. Read-only and
// machine-scoped; a client asks each machine's own daemon over its link.
func registerJobResourceHandler(hub *wire.Hub, manager *acp.Manager) {
	hub.Register("resources:list", func(args []any) (any, error) {
		queues := manager.ResourceQueues()
		return map[string]any{"resources": resourceOverview(procchild.Resources(manager.StateDir()), queues)}, nil
	})
}
