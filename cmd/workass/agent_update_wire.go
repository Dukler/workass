package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

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
func registerAgentUpdateWireHandlers(hub *wire.Hub, renderer agentChatRemoteRouter, selfMachineID string) {
	if hub == nil {
		return
	}
	hub.RegisterOutOfBandRead("app:update-status", func(args []any) (any, error) {
		params := firstMapArg(args)
		if err := validateAgentUpdateTarget(params, selfMachineID); err != nil {
			return nil, err
		}
		if renderer == nil {
			return nil, errors.New("Workass updater status requires the local Electron renderer")
		}
		return renderer.Call(context.Background(), "update.local.status", copyRemoteAgentRouteParams(params))
	})
	hub.Register("app:update-apply", func(args []any) (any, error) {
		params := firstMapArg(args)
		if err := validateAgentUpdateApply(params, selfMachineID); err != nil {
			return nil, err
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
