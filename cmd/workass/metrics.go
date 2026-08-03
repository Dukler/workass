package main

import (
	"os"
	"path/filepath"
	"sort"

	"workass/internal/acp"
	"workass/internal/wire"
)

// Daemon-owned counters for GET /workass/metrics. The renderer boot that pulled
// every chat's archive through this process took it from 38MB to 614MB resident
// and nothing reported that, so "the app is slow" had no measurable backend
// side. Everything here is a stat or a walk over already-resident state: the
// endpoint must never become the reason the daemon is busy.
func daemonMetrics(store *sessionStore, stateDir string, hub *wire.Hub, manager *acp.Manager) map[string]any {
	out := map[string]any{}

	if hub != nil {
		out["wire"] = hub.Stats()
	}
	if manager != nil {
		out["acp"] = manager.Stats()
	}

	if store != nil {
		session := store.Inventory()
		session["snapshotBytes"] = fileBytes(filepath.Join(stateDir, "session-state.json"))
		out["session"] = session
	}

	// Archive volume is the single best predictor of a memory storm: answering
	// chat:archive-load materializes the file into Go maps and marshals it
	// again for the wire, several times its size on disk.
	archiveDir := filepath.Join(stateDir, "chat-archive")
	if entries, err := os.ReadDir(archiveDir); err == nil {
		type archiveInfo struct {
			name  string
			bytes int64
		}
		infos := make([]archiveInfo, 0, len(entries))
		var total int64
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			total += info.Size()
			infos = append(infos, archiveInfo{name: entry.Name(), bytes: info.Size()})
		}
		sort.Slice(infos, func(a, b int) bool { return infos[a].bytes > infos[b].bytes })
		largest := make([]map[string]any, 0, 3)
		for _, info := range infos {
			if len(largest) == 3 {
				break
			}
			largest = append(largest, map[string]any{"file": info.name, "bytes": info.bytes})
		}
		out["archives"] = map[string]any{
			"files":      len(infos),
			"totalBytes": total,
			"largest":    largest,
		}
	}

	return out
}

func fileBytes(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
