package main

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	"workass/internal/wire"
)

// Fork tools: an agent in any owned chat marks a fork point on an exact
// target chat and fans out independent children from it. The fork service
// keeps its own durable operation receipts (same id + same input replays the
// original result), so these calls do not enter the caller's actor outbox.

func (h *agentControlHandler) callFork(r *http.Request, method string, params map[string]any, ownerKey, callerTab, callerChat string) (any, error) {
	if h.forks == nil {
		return nil, errors.New("forkable subsessions are unavailable in this Workass daemon")
	}
	if err := h.authorizeActorOwner(ownerKey, callerTab, callerChat, "Workass fork caller is not an owned ACP session"); err != nil {
		return nil, err
	}
	switch method {
	case "fork.create":
		tabID, chatID, err := exactAgentTarget(params)
		if err != nil {
			return nil, err
		}
		parent, err := h.forkParent(tabID, chatID)
		if err != nil {
			return nil, err
		}
		point, err := h.forks.CreateForkPoint(r.Context(), parent, acp.ForkPointOptions{
			Name: fieldString(params, "name"), OperationID: fieldString(params, "operation_id"),
			ModeID: fieldString(params, "mode_id"), Effort: fieldString(params, "effort"), AddDirs: stringSliceField(params["add_dirs"]),
		})
		if err != nil {
			if point.ID != "" {
				return nil, errors.New("fork point " + point.ID + " failed: " + err.Error())
			}
			return nil, err
		}
		return map[string]any{"ok": true, "forkPoint": acp.ForkPointView(point)}, nil
	case "fork.list":
		tabID, chatID, err := exactAgentTarget(params)
		if err != nil {
			return nil, err
		}
		if err := h.fenceActor(tabID, chatID); err != nil {
			return nil, err
		}
		return h.forkListView(chatID)
	case "fork.run":
		pointID := fieldString(params, "fork_point_id")
		if err := h.fenceForkOwner(h.forks.PointOwner(pointID)); err != nil {
			return nil, err
		}
		opts := acp.ForkRunOptions{
			ForkPointID: pointID, Concurrency: intField(params, "concurrency"),
			Retention: fieldString(params, "retention"), RetentionDays: intField(params, "retention_days"),
			ChildTimeout: time.Duration(intField(params, "child_timeout_ms")) * time.Millisecond,
			MaxImageEdge: intField(params, "max_image_edge_px"), OperationID: fieldString(params, "operation_id"),
			RequesterChat: callerChat, RequesterTabID: callerTab,
		}
		if value, ok := boolField(params, "label_images"); ok {
			opts.LabelImages = &value
		}
		for _, raw := range anySlice(params["children"]) {
			child := mapFromAnyMain(raw)
			opts.Children = append(opts.Children, acp.ForkChildSpec{
				Label: fieldString(child, "label"), Message: fieldString(child, "message"),
				Attachments: stringSliceField(child["attachments"]),
			})
		}
		run, err := h.forks.StartRun(opts)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "run": acp.ForkRunView(run)}, nil
	case "fork.wait":
		runID := fieldString(params, "run_id")
		if err := h.fenceForkOwner(h.forks.RunOwner(runID)); err != nil {
			return nil, err
		}
		timeoutMS := intField(params, "timeout_ms")
		if timeoutMS != -1 && timeoutMS != 0 && (timeoutMS < 1000 || timeoutMS > 3600000) {
			return nil, errors.New("timeout_ms must be -1 (until settled), 0 (default 10 minutes), or between 1000 and 3600000")
		}
		timeout := time.Duration(timeoutMS) * time.Millisecond
		if timeoutMS == 0 {
			timeout = 10 * time.Minute
		}
		if timeoutMS == -1 {
			timeout = -1
		}
		run, settled, err := h.forks.Wait(r.Context(), runID, stringSliceField(params["child_ids"]), fieldString(params, "return_when"), timeout)
		if err != nil && run.ID == "" {
			return nil, err
		}
		reply := map[string]any{"ok": err == nil, "settled": settled, "run": acp.ForkRunView(run)}
		if err != nil {
			reply["error"] = err.Error()
		}
		return reply, nil
	case "fork.cancel":
		runID := fieldString(params, "run_id")
		if err := h.fenceForkOwner(h.forks.RunOwner(runID)); err != nil {
			return nil, err
		}
		child, err := h.forks.CancelChild(runID, fieldString(params, "child_id"), fieldString(params, "operation_id"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "child": acp.ForkChildView(child)}, nil
	case "fork.retry":
		runID := fieldString(params, "run_id")
		if err := h.fenceForkOwner(h.forks.RunOwner(runID)); err != nil {
			return nil, err
		}
		child, err := h.forks.RetryChild(runID, fieldString(params, "child_id"), fieldString(params, "message"), fieldString(params, "operation_id"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "child": acp.ForkChildView(child)}, nil
	}
	return nil, errors.New("unknown fork method")
}

func (h *agentControlHandler) fenceForkOwner(chatID, tabID string, ok bool) error {
	if !ok {
		return errors.New("unknown fork point or run; call workass_list_forks for exact ids")
	}
	return h.fenceActor(tabID, chatID)
}

func (h *agentControlHandler) forkListView(chatID string) (map[string]any, error) {
	points, runs, err := h.forks.List(chatID)
	if err != nil {
		return nil, err
	}
	pointViews := make([]any, 0, len(points))
	for _, point := range points {
		pointViews = append(pointViews, acp.ForkPointView(point))
	}
	runViews := make([]any, 0, len(runs))
	for _, run := range runs {
		runViews = append(runViews, acp.ForkRunView(run))
	}
	return map[string]any{"ok": true, "forkPoints": pointViews, "runs": runViews}, nil
}

// forkParent snapshots the target chat at the mark. The chat must be idle so
// its native transcript is not being appended while the seal forks it.
func (h *agentControlHandler) forkParent(tabID, chatID string) (acp.ForkParent, error) {
	if h.chats == nil || h.chats.providerChats == nil {
		return acp.ForkParent{}, errors.New("fork points require the durable chat actor")
	}
	_, state, err := h.chats.providerChats.exactActor(tabID, chatID)
	if err != nil {
		return acp.ForkParent{}, err
	}
	return forkParentFromState(tabID, chatID, state)
}

func forkParentFromState(tabID, chatID string, state chat.State) (acp.ForkParent, error) {
	if state.Foreground != nil || len(state.Queue) > 0 {
		return acp.ForkParent{}, errors.New("the chat is busy; mark the fork point once its turn and queue have finished")
	}
	lane, ok := state.Lanes[state.ActiveLaneID]
	if !ok {
		return acp.ForkParent{}, errors.New("the chat has no provider conversation yet; send it a message before marking a fork point")
	}
	thread := lane.Thread.Normalize()
	sessionID := thread.HeadID
	if sessionID == "" {
		sessionID = thread.RootID
	}
	providerID := string(state.Presentation.ProviderID)
	if providerID == "" {
		providerID = string(thread.ProviderID)
	}
	modelID := strings.TrimSpace(firstNonEmptyString(state.Presentation.CurrentModelID, lane.ModelID))
	baseModel, effort, split := acp.SplitCanonicalEffortSuffix(modelID)
	if !split {
		baseModel, effort = modelID, ""
	}
	cwd := strings.TrimSpace(lane.CWD)
	if state.Presentation.CWD != nil && strings.TrimSpace(*state.Presentation.CWD) != "" {
		cwd = strings.TrimSpace(*state.Presentation.CWD)
	}
	afterMessageID := ""
	for index := len(state.Ledger) - 1; index >= 0; index-- {
		if id := strings.TrimSpace(state.Ledger[index].MessageID); id != "" {
			afterMessageID = id
			break
		}
	}
	return acp.ForkParent{
		ChatID: chatID, TabID: tabID, ProviderID: providerID, ModelID: baseModel, Effort: effort,
		ModeID: firstNonEmptyString(state.Presentation.CurrentModeID, lane.ModeID), CWD: cwd,
		NativeSessionID: sessionID, AfterMessageID: afterMessageID,
	}, nil
}

// registerForkWireHandlers exposes fork state to the renderer. These are
// additive channels: an older renderer simply never calls them.
func registerForkWireHandlers(hub *wire.Hub, forks *acp.ForkService) {
	hub.Register("fork:list", func(args []any) (any, error) {
		arg := firstMapArg(args)
		chatID := fieldString(arg, "chatId")
		if chatID == "" {
			return nil, errors.New("fork:list requires {tabId, chatId}")
		}
		handler := &agentControlHandler{forks: forks}
		return handler.forkListView(chatID)
	})
	hub.Register("fork:summary", func(args []any) (any, error) {
		return map[string]any{"ok": true, "chats": forks.Summary()}, nil
	})
	hub.Register("fork:transcript", func(args []any) (any, error) {
		arg := firstMapArg(args)
		entries, err := forks.Transcript(fieldString(arg, "runId"), fieldString(arg, "childId"))
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}, nil
		}
		return map[string]any{"ok": true, "entries": entries}, nil
	})
	hub.Register("fork:cancel-child", func(args []any) (any, error) {
		arg := firstMapArg(args)
		child, err := forks.CancelChild(fieldString(arg, "runId"), fieldString(arg, "childId"), fieldString(arg, "operationId"))
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}, nil
		}
		return map[string]any{"ok": true, "child": acp.ForkChildView(child)}, nil
	})
	hub.Register("fork:image", func(args []any) (any, error) {
		arg := firstMapArg(args)
		attachment, data, err := forks.ChildImage(fieldString(arg, "runId"), fieldString(arg, "childId"), intField(arg, "index"))
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}, nil
		}
		return map[string]any{
			"ok": true, "name": attachment.Name, "mimeType": attachment.MIME, "data": data,
			"width": attachment.Width, "height": attachment.Height,
		}, nil
	})
	hub.Register("fork:retain", func(args []any) (any, error) {
		arg := firstMapArg(args)
		retention := acp.ForkRetentionThrowaway
		if keep, _ := boolField(arg, "keep"); keep {
			retention = acp.ForkRetentionKeep
		}
		run, err := forks.SetRunRetention(fieldString(arg, "runId"), retention)
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}, nil
		}
		return map[string]any{"ok": true, "run": acp.ForkRunView(run)}, nil
	})
	hub.Register("fork:retry-child", func(args []any) (any, error) {
		arg := firstMapArg(args)
		child, err := forks.RetryChild(fieldString(arg, "runId"), fieldString(arg, "childId"), fieldString(arg, "message"), fieldString(arg, "operationId"))
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}, nil
		}
		return map[string]any{"ok": true, "child": acp.ForkChildView(child)}, nil
	})
}

func newForkService(stateDir string, manager *acp.Manager, broadcast func(string, any)) *acp.ForkService {
	return acp.NewForkService(stateDir, manager.ForkLaunch, func(tabID, chatID string) {
		if broadcast != nil {
			broadcast("fork:changed", map[string]any{"tabId": tabID, "chatId": chatID})
		}
	})
}
