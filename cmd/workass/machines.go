package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"workass/internal/machinebook"
	"workass/internal/machineid"
	"workass/internal/wire"
)

// machineChannelCount is how many wire channels the machine book adds. Headless
// first, per E1: the book is provable from a CLI before a pixel exists.
const machineChannelCount = 5

// machineRefreshInterval matches discovery, so a machine that dies is marked
// unreachable within one interval rather than one of two competing clocks.
const machineRefreshInterval = machinebook.DefaultInterval

// openMachineBook loads this daemon's machine list. A daemon with no provable
// identity gets no book: it could not tell a machine apart from itself, and an
// entry it wrote could not be trusted to name anything stable.
func openMachineBook(stateDir string, identity machineid.Identity, logger *log.Logger) *machinebook.Book {
	if strings.TrimSpace(identity.MachineID) == "" {
		return nil
	}
	book, err := machinebook.Open(machinebook.Options{
		StateDir:    stateDir,
		SelfID:      identity.MachineID,
		WireVersion: daemonWireVersion,
	})
	if err != nil {
		// Deliberately not self-healing, for the same reason the identity file
		// is not: replacing an unreadable book would drop every machine the
		// user configured without telling them which ones.
		logger.Printf("[workass] machine book unavailable: %v", err)
		return nil
	}
	return book
}

// registerMachineHandlers puts the book on the wire.
//
// Failures a human caused — an address that never answered, a web server where
// a daemon was expected, this machine's own address — come back as results
// rather than errors, because they belong next to the field that was typed
// rather than in a toast that outlives it.
func registerMachineHandlers(hub *wire.Hub, book *machinebook.Book, identity machineid.Identity) int {
	self := map[string]any{
		"machineId":   identity.MachineID,
		"name":        identity.DisplayName,
		"wireVersion": daemonWireVersion,
		"secure":      true,
		"self":        true,
	}
	snapshot := func() map[string]any {
		return map[string]any{"machines": book.List(), "self": self}
	}

	hub.Register("machines:list", func(args []any) (any, error) {
		return snapshot(), nil
	})
	hub.Register("machines:add", func(args []any) (any, error) {
		address := fieldString(firstMapArg(args), "address")
		if strings.TrimSpace(address) == "" {
			return map[string]any{"ok": false, "error": "type an address, like 192.168.1.50:8788"}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		entry, err := book.Add(ctx, address)
		if err != nil {
			return map[string]any{"ok": false, "error": machineAddError(err), "machines": book.List(), "self": self}, nil
		}
		result := snapshot()
		result["ok"] = true
		result["machine"] = entry
		hub.Broadcast("machines:changed", snapshot())
		return result, nil
	})
	hub.Register("machines:forget", func(args []any) (any, error) {
		machineID := fieldString(firstMapArg(args), "machineId")
		forgotten, err := book.Forget(machineID)
		if err != nil {
			return nil, err
		}
		result := snapshot()
		result["ok"] = forgotten
		if forgotten {
			hub.Broadcast("machines:changed", snapshot())
		}
		return result, nil
	})
	hub.Register("machines:nickname", func(args []any) (any, error) {
		arg := firstMapArg(args)
		entry, changed, err := book.SetNickname(fieldString(arg, "machineId"), fieldString(arg, "nickname"))
		result := snapshot()
		if err != nil {
			result["ok"] = false
			result["error"] = err.Error()
			return result, nil
		}
		result["ok"] = true
		result["machine"] = entry
		if changed {
			hub.Broadcast("machines:changed", snapshot())
		}
		return result, nil
	})
	hub.Register("machines:refresh", func(args []any) (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		machines, changed := book.Refresh(ctx)
		if changed {
			hub.Broadcast("machines:changed", snapshot())
		}
		return map[string]any{"machines": machines, "self": self, "ok": true}, nil
	})
	return machineChannelCount
}

// machineAddError turns a probe failure into something worth reading. The
// address in the message is the one that was actually tried, which is often the
// answer by itself — a typed hostname that resolved somewhere unexpected.
func machineAddError(err error) string {
	switch {
	case errors.Is(err, machinebook.ErrSelf):
		return "that address is this machine"
	case errors.Is(err, machinebook.ErrNotWorkass):
		return "something answered there, but it is not a workass daemon"
	case errors.Is(err, machinebook.ErrNoIdentity):
		return "that daemon is too old to be added — update it"
	default:
		return err.Error()
	}
}

// beaconDefault enables the current private-LAN discovery probe by default.
func beaconDefault() bool {
	return true
}

// machinePresenceOptions is how the daemon is exposed to other machines.
type machinePresenceOptions struct {
	Bind   string
	Port   int
	Beacon bool
}

// startMachinePresence runs TCP-port-80 discovery and liveness until ctx ends.
// It never sends multicast or broadcast traffic. Bind controls whether this
// daemon itself is reachable; discovery is client-side and works either way.
func startMachinePresence(ctx context.Context, book *machinebook.Book, hub *wire.Hub, identity machineid.Identity, opts machinePresenceOptions, logger *log.Logger) {
	if book == nil {
		return
	}
	broadcast := func() {
		hub.Broadcast("machines:changed", map[string]any{
			"machines": book.List(),
			"self": map[string]any{
				"machineId":   identity.MachineID,
				"name":        identity.DisplayName,
				"wireVersion": daemonWireVersion,
				"secure":      true,
				"self":        true,
			},
		})
	}

	go func() {
		ticker := time.NewTicker(machineRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, changed := book.Refresh(ctx); changed {
					broadcast()
				}
			}
		}
	}()

	if !opts.Beacon {
		logger.Printf("[workass] automatic port-80 discovery is disabled (--beacon=false)")
		return
	}
	scanner := &machinebook.Scanner{
		Book:     book,
		OnChange: func(machinebook.Entry) { broadcast() },
		Logf:     logger.Printf,
	}
	go func() {
		if err := scanner.Run(ctx); err != nil {
			logger.Printf("[workass] automatic port-80 discovery stopped: %v", err)
		}
	}()
	logger.Printf("[workass] automatic machine discovery probing private LAN hosts on TCP port %d", machinebook.DiscoveryPort)
}
