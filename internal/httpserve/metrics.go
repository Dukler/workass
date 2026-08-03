package httpserve

import (
	"encoding/json"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strings"
	"time"
)

// MetricsFunc supplies daemon-owned counters — session size, archive bytes,
// engine inventory. httpserve contributes only process and runtime facts so it
// stays free of dependencies on the session store or the ACP manager.
type MetricsFunc func() map[string]any

const (
	metricsPath    = "/workass/metrics"
	pprofPathRoot  = "/debug/pprof"
	pprofEnableEnv = "WORKASS_PPROF"
)

var processStart = time.Now()

// A renderer boot that asked for every chat's archive took this daemon from
// 38MB to 614MB of resident memory, and Go keeps that heap. Nothing in the
// daemon reported its own size, so the only symptom was "the app feels slow".
// These counters exist so that class of problem is a query, not a hunch.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	// Diagnostics name chats and payload sizes: loopback only, never a paired
	// LAN client.
	if !IsLocalIP(ClientIP(r.RemoteAddr)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	lastPauseMs := 0.0
	if mem.NumGC > 0 {
		lastPauseMs = float64(mem.PauseNs[(mem.NumGC+255)%256]) / 1e6
	}
	out := map[string]any{
		"app":           "workass",
		"uptimeSeconds": int64(time.Since(processStart).Seconds()),
		"goroutines":    runtime.NumGoroutine(),
		"heap": map[string]any{
			"allocBytes":    mem.HeapAlloc,
			"sysBytes":      mem.HeapSys,
			"idleBytes":     mem.HeapIdle,
			"releasedBytes": mem.HeapReleased,
			"objects":       mem.HeapObjects,
		},
		"totalSysBytes": mem.Sys,
		"gc": map[string]any{
			"count":        mem.NumGC,
			"pauseTotalMs": float64(mem.PauseTotalNs) / 1e6,
			"lastPauseMs":  lastPauseMs,
		},
		"pprofEnabled": pprofEnabled(),
	}
	if peak, ok := peakRSSBytes(); ok {
		// Peak, not current: Go returns freed pages to the OS lazily, so the
		// high-water mark is what exposes a transient allocation storm.
		out["rssPeakBytes"] = peak
	}
	if s.Metrics != nil {
		for key, value := range s.Metrics() {
			out[key] = value
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", " ")
	_ = encoder.Encode(out)
}

func pprofEnabled() bool {
	return strings.TrimSpace(os.Getenv(pprofEnableEnv)) == "1"
}

// Profiling is opt-in and loopback-only: a heap profile is the only way to
// answer "what is holding that memory", but it also dumps allocation sites.
func (s *Server) servePprof(w http.ResponseWriter, r *http.Request) {
	if !pprofEnabled() || !IsLocalIP(ClientIP(r.RemoteAddr)) {
		http.NotFound(w, r)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, pprofPathRoot+"/") {
	case "cmdline":
		pprof.Cmdline(w, r)
	case "profile":
		pprof.Profile(w, r)
	case "symbol":
		pprof.Symbol(w, r)
	case "trace":
		pprof.Trace(w, r)
	default:
		pprof.Index(w, r)
	}
}
