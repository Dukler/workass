package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"workass/internal/machinebook"
	"workass/internal/machineid"
	"workass/internal/wire"
)

// machineChannelCount is how many wire channels the machine book adds. Headless
// first, per E1: the book is provable from a CLI before a pixel exists.
const machineChannelCount = 4

// machineRefreshInterval matches the beacon's, so a machine that dies is marked
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
		"secure":      false,
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

// beaconDefault reads the profile's presence setting.
//
// Reachable and announcing are two different decisions. On a managed machine,
// endpoint security watches for processes that start speaking to the network
// on their own, so being able to open the port without also broadcasting is
// worth a flag. Announcing stays the default everywhere else — a machine you
// opened deliberately is one you want found.
func beaconDefault() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WORKASS_DAEMON_BEACON"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// machinePresenceOptions is how the daemon is exposed to other machines.
type machinePresenceOptions struct {
	Bind   string
	Port   int
	Beacon bool
}

// startMachinePresence runs discovery and liveness until ctx ends.
//
// The beacon only runs on a LAN-bound daemon. A daemon listening on loopback is
// not reachable by anyone, so announcing an address for it would be advertising
// a door that does not exist. Liveness runs either way: machines added by hand
// are ones the user asked about, and their status is owed regardless of whether
// this daemon announces itself.
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
				"secure":      false,
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

	switch {
	case opts.Bind != "lan":
		logger.Printf("[workass] machine beacon off (bind=%s); machines can still be added by address", opts.Bind)
		return
	case !opts.Beacon:
		logger.Printf("[workass] machine beacon off (--beacon=false); this machine is reachable but does not announce itself")
		return
	}
	beacon := &machinebook.Beacon{
		Book:      book,
		MachineID: identity.MachineID,
		Name:      identity.DisplayName,
		Port:      opts.Port,
		OnChange:  func(machinebook.Entry) { broadcast() },
		Logf:      logger.Printf,
	}
	go func() {
		if err := beacon.Run(ctx); err != nil {
			logger.Printf("[workass] machine beacon stopped: %v", err)
		}
	}()
	logger.Printf("[workass] machine beacon announcing %s on %s:%d", identity.MachineID, machinebook.GroupIP, machinebook.GroupPort)
}
