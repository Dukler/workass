package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"workass/internal/headlessupdate"
)

// headlessUpdateService is the daemon-side half of the updater contract for an
// install that has no Electron shell to ask: a Linux `systemd --user` service.
//
// The methods are the same three the renderer answers — update.targets,
// update.status, update.apply — so `workass_list_update_targets`,
// `workass_get_update_status` and `workass_apply_update` behave identically
// whether the addressed machine is a packaged app or a headless daemon. Only
// the mechanism below the method differs.
type headlessUpdateService struct {
	// Deliberately not named `manager`: in cmd/workass that identifier means
	// the ACP Manager, and the Phase C authority contract reads it as one.
	updates  *headlessupdate.Manager
	machine  string
	name     string
	restart  func()
	exitWith atomic.Bool
}

func newHeadlessUpdateService(options headlessupdate.Options, machineID, machineName string) *headlessUpdateService {
	service := &headlessUpdateService{machine: strings.TrimSpace(machineID), name: strings.TrimSpace(machineName)}
	options.RequestRestart = func() {
		service.exitWith.Store(true)
		if service.restart != nil {
			service.restart()
		}
	}
	service.updates = headlessupdate.New(options)
	return service
}

// SetMachine binds this updater to the daemon's proven machine identity. The
// identity is read after the flags, so it arrives here rather than at
// construction; until it does, no update request can address this machine.
func (s *headlessUpdateService) SetMachine(machineID, machineName string) {
	if s == nil {
		return
	}
	s.machine = strings.TrimSpace(machineID)
	s.name = strings.TrimSpace(machineName)
}

// SetRunningJobs tells the updater how to read this machine's running jobs. It
// arrives here because the manager is built after the updater.
func (s *headlessUpdateService) SetRunningJobs(read func() []headlessupdate.RunningJob) {
	if s != nil && s.updates != nil {
		s.updates.SetRunningJobs(read)
	}
}

// SetRestart hands the updater the daemon's own graceful-shutdown authority.
// The updater never kills a process and never calls systemctl restart: it
// replaces files, asks for the ordinary shutdown, and lets the unit's
// Restart= policy start the new binary. Engines settle exactly as they do on
// any other stop.
func (s *headlessUpdateService) SetRestart(requestShutdown func()) {
	if s == nil {
		return
	}
	s.restart = requestShutdown
}

// RestartForUpdate reports whether this daemon stopped in order to be replaced,
// which is what makes main exit with a code the unit restarts on.
func (s *headlessUpdateService) RestartForUpdate() bool {
	return s != nil && s.exitWith.Load()
}

// Settle runs the boot arbiter. It returns true when this boot has to exit
// again so systemd starts the binary it just rolled back to.
func (s *headlessUpdateService) Settle() bool {
	if s == nil || s.updates == nil {
		return false
	}
	return s.updates.Settle()
}

// Run drives the background release-feed discovery. Nothing here activates
// anything: it only keeps availableVersion current so the read tools can
// answer without performing network I/O of their own.
func (s *headlessUpdateService) Run(ctx context.Context) {
	if s == nil || s.updates == nil {
		return
	}
	s.updates.Run(ctx)
}

// Confirm marks the activation healthy once this daemon is actually serving.
func (s *headlessUpdateService) Confirm() {
	if s == nil || s.updates == nil {
		return
	}
	s.updates.Confirm()
}

func (s *headlessUpdateService) unsupportedReason() string {
	if !s.available() {
		return "this Workass daemon has no updater"
	}
	return s.updates.UnsupportedReason()
}

func (s *headlessUpdateService) available() bool {
	return s != nil && s.updates != nil
}

// Targets answers update.targets. A headless daemon has no renderer and
// therefore no mounted-machine book to project, so it names exactly the one
// machine it can speak for: itself.
func (s *headlessUpdateService) Targets(ctx context.Context) (any, error) {
	if !s.available() {
		return nil, errors.New("this Workass daemon has no updater")
	}
	status := s.updates.Status(ctx)
	state, _ := status["state"].(map[string]any)
	name := s.name
	if name == "" {
		name = "This machine"
	}
	target := map[string]any{
		"machineId":           s.machine,
		"name":                name,
		"local":               true,
		"link":                "ready",
		"headless":            true,
		"strategy":            headlessupdate.Strategy,
		"updaterMcpSupported": true,
	}
	if state != nil {
		target["currentVersion"] = state["currentVersion"]
		target["targetVersion"] = state["targetVersion"]
		target["availableVersion"] = state["availableVersion"]
		target["phase"] = state["phase"]
		target["supported"] = state["supported"]
		if reason, ok := state["error"].(string); ok && reason != "" {
			target["reason"] = reason
		}
	}
	return map[string]any{
		"selfMachineId": s.machine,
		"targets":       []any{target},
		// Said plainly rather than implied by an empty list: this daemon is not
		// the renderer, so it cannot enumerate the machines a client has mounted.
		"remoteAvailable": false,
		"remoteError":     "a headless Workass daemon reports only its own update target",
	}, nil
}

// Status answers update.status for this exact machine.
func (s *headlessUpdateService) Status(ctx context.Context, params map[string]any) (any, error) {
	if !s.available() {
		return nil, errors.New("this Workass daemon has no updater")
	}
	if err := s.requireThisMachine(params); err != nil {
		return nil, err
	}
	return map[string]any{
		"machineId":   s.machine,
		"local":       true,
		"headless":    true,
		"diagnostics": s.updates.Status(ctx),
	}, nil
}

// Apply answers update.apply. Only a human order relayed for this exact
// machine, version pair and operation_id reaches here; the daemon never
// schedules, retries or infers an activation.
func (s *headlessUpdateService) Apply(ctx context.Context, params map[string]any) (any, error) {
	if !s.available() {
		return nil, errors.New("this Workass daemon has no updater")
	}
	if err := s.requireThisMachine(params); err != nil {
		return nil, err
	}
	state, err := s.updates.Apply(ctx, headlessupdate.ApplyRequest{
		OperationID:            fieldString(params, "operation_id"),
		MachineID:              strings.TrimSpace(fieldString(params, "machine_id")),
		ExpectedCurrentVersion: fieldString(params, "expected_current_version"),
		ExpectedTargetVersion:  fieldString(params, "expected_target_version"),
		Authorization:          fieldString(params, "authorization"),
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"machineId": s.machine, "local": true, "headless": true, "state": state}, nil
}

func (s *headlessUpdateService) requireThisMachine(params map[string]any) error {
	requested := strings.TrimSpace(fieldString(params, "machine_id"))
	if requested == "" {
		return errors.New("update request requires an exact machine_id")
	}
	if s.machine == "" || requested != s.machine {
		return errors.New("update request does not address this exact Workass machine")
	}
	return nil
}

// headlessUpdateCall routes one updater method to the daemon-side updater.
func (s *headlessUpdateService) headlessUpdateCall(ctx context.Context, method string, params map[string]any) (any, error) {
	switch method {
	case "update.targets":
		return s.Targets(ctx)
	case "update.status", "update.local.status":
		return s.Status(ctx, params)
	case "update.apply", "update.local.apply":
		return s.Apply(ctx, params)
	}
	return nil, errors.New("unknown Workass updater method " + method)
}
