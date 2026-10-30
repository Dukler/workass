package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"workass/internal/acp"
	"workass/internal/headlessupdate"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

const (
	agentUpdateStatusChannel = "app:update-status"
	agentUpdateApplyChannel  = "app:update-apply"
)

var updateVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// registerAgentUpdateWireHandlers gives an authenticated remote controller the
// same exact-machine updater surface as a local agent MCP call. The daemon does
// not touch installation files: it asks this machine's stamped Electron
// renderer, which in turn invokes the existing transactional UpdateManager.
//
// A headless install has no renderer at all, so when one is present it owns
// both channels instead. The channel names, payloads and authorization text
// are identical either way: a controller on another machine cannot tell, and
// does not need to, which half of the fleet answers.
func registerAgentUpdateWireHandlers(hub *wire.Hub, renderer agentChatRemoteRouter, headless *headlessUpdateService, selfMachineID string, runningJobs func() []headlessupdate.RunningJob) {
	if hub == nil {
		return
	}
	hub.RegisterOutOfBandRead("app:update-status", func(args []any) (any, error) {
		params := firstMapArg(args)
		if err := validateAgentUpdateTarget(params, selfMachineID); err != nil {
			return nil, err
		}
		if headless.available() {
			return headless.headlessUpdateCall(context.Background(), "update.local.status", params)
		}
		if renderer == nil {
			return nil, errors.New("Workass updater status requires the local Electron renderer")
		}
		result, err := renderer.Call(context.Background(), "update.local.status", copyRemoteAgentRouteParams(params))
		if err == nil && runningJobs != nil {
			result = annotateRunningJobs(result, runningJobs())
		}
		return result, err
	})
	hub.Register("app:update-apply", func(args []any) (any, error) {
		params := firstMapArg(args)
		if err := validateAgentUpdateApply(params, selfMachineID); err != nil {
			return nil, err
		}
		if headless.available() {
			return headless.headlessUpdateCall(context.Background(), "update.local.apply", params)
		}
		if renderer == nil {
			return nil, errors.New("Workass update activation requires the local Electron renderer")
		}
		return renderer.Call(context.Background(), "update.local.apply", copyRemoteAgentRouteParams(params))
	})
}

func validateAgentUpdateTarget(params map[string]any, selfMachineID string) error {
	machineID := strings.TrimSpace(fieldString(params, "machine_id"))
	selfMachineID = strings.TrimSpace(selfMachineID)
	if machineID == "" {
		return errors.New("update request requires an exact machine_id")
	}
	if selfMachineID == "" || machineID != selfMachineID {
		return errors.New("update request does not address this exact Workass machine")
	}
	return nil
}

func validateAgentUpdateApply(params map[string]any, selfMachineID string) error {
	if err := validateAgentUpdateTarget(params, selfMachineID); err != nil {
		return err
	}
	if _, err := providercontract.ValidateOperationID(fieldString(params, "operation_id")); err != nil {
		return fmt.Errorf("update %w", err)
	}
	currentVersion := strings.TrimSpace(fieldString(params, "expected_current_version"))
	targetVersion := strings.TrimSpace(fieldString(params, "expected_target_version"))
	if !updateVersionPattern.MatchString(currentVersion) || !updateVersionPattern.MatchString(targetVersion) {
		return errors.New("update request requires exact X.Y.Z current and target versions")
	}
	machineID := strings.TrimSpace(fieldString(params, "machine_id"))
	expectedAuthorization := fmt.Sprintf("update %s from %s to %s", machineID, currentVersion, targetVersion)
	if fieldString(params, "authorization") != expectedAuthorization {
		return errors.New("update authorization does not exactly match the machine and versions")
	}
	return nil
}

// annotateRunningJobs adds the machine's running jobs to a status the renderer
// answered. The daemon holds the jobs and the renderer holds the updater, so this
// is where the two facts meet on a packaged install. Additive: a status that is
// not an object is passed through.
func annotateRunningJobs(result any, jobs []headlessupdate.RunningJob) any {
	fields := mapFromAnyMain(result)
	if len(fields) == 0 {
		return result
	}
	out := copyAnyMap(fields)
	out["runningJobs"] = headlessupdate.DescribeRunningJobs(jobs)
	return out
}

// runningJobsForUpdate reads this machine's running jobs in the updater's words.
func runningJobsForUpdate(jobs []acp.JobSurvival) []headlessupdate.RunningJob {
	out := make([]headlessupdate.RunningJob, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, headlessupdate.RunningJob(job))
	}
	return out
}

// withRunningJobs is annotateRunningJobs for the tool path.
func (h *agentControlHandler) withRunningJobs(result any) any {
	if h == nil || h.manager == nil {
		return result
	}
	return annotateRunningJobs(result, runningJobsForUpdate(h.manager.RunningJobSurvival()))
}
