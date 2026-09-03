package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"workass/internal/wire"
)

const (
	remoteAgentRouteRequestChannel  = "agent:route-request"
	remoteAgentRouteResponseChannel = "agent:route-response"
	remoteAgentRouteTimeout         = 2 * time.Minute
	machineTaggedIDPrefix           = "M~"
)

type agentChatRemoteRouter interface {
	Call(context.Context, string, map[string]any) (any, error)
}

type remoteAgentRouteReply struct {
	result any
	err    error
}

// rendererAgentRouter is the narrow bridge between the daemon-hosted MCP and
// the renderer-owned machine registry. Per-machine device credentials remain
// inside the renderer; the MCP never receives or persists them.
type rendererAgentRouter struct {
	mu        sync.Mutex
	pending   map[string]chan remoteAgentRouteReply
	broadcast func(string, any) int
	timeout   time.Duration
}

func registerRendererAgentRouter(hub *wire.Hub) *rendererAgentRouter {
	if hub == nil {
		return nil
	}
	router := &rendererAgentRouter{
		pending:   make(map[string]chan remoteAgentRouteReply),
		broadcast: hub.BroadcastLocalShell,
		timeout:   remoteAgentRouteTimeout,
	}
	// A route response must be able to complete an MCP call while the renderer's
	// ordinary ordered socket lane is busy. It mutates no Workass state itself;
	// the addressed operation already ran through the destination daemon.
	hub.RegisterOutOfBandRead(remoteAgentRouteResponseChannel, router.resolve)
	return router
}

func (r *rendererAgentRouter) Call(ctx context.Context, method string, params map[string]any) (any, error) {
	if r == nil || r.broadcast == nil {
		return nil, errors.New("Workass remote-chat MCP router is unavailable")
	}
	method = strings.TrimSpace(method)
	if method == "" {
		return nil, errors.New("remote-chat MCP route requires a method")
	}
	requestID, err := newRemoteAgentRouteID()
	if err != nil {
		return nil, fmt.Errorf("create remote-chat route id: %w", err)
	}
	wait := r.timeout
	if wait <= 0 {
		wait = remoteAgentRouteTimeout
	}
	routeCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	replies := make(chan remoteAgentRouteReply, 1)
	r.mu.Lock()
	r.pending[requestID] = replies
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, requestID)
		r.mu.Unlock()
	}()

	payload := map[string]any{
		"requestId": requestID,
		"method":    method,
		"params":    copyRemoteAgentRouteParams(params),
		"expiresAt": time.Now().Add(wait).UnixMilli(),
	}
	if delivered := r.broadcast(remoteAgentRouteRequestChannel, payload); delivered == 0 {
		return nil, errors.New("Workass remote-chat MCP router has no local renderer")
	}
	select {
	case <-routeCtx.Done():
		if errors.Is(routeCtx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("Workass remote-chat MCP router timed out")
		}
		return nil, routeCtx.Err()
	case reply := <-replies:
		return reply.result, reply.err
	}
}

func (r *rendererAgentRouter) resolve(args []any) (any, error) {
	params := firstMapArg(args)
	requestID := fieldString(params, "requestId")
	if requestID == "" {
		return nil, errors.New("remote-chat route response requires requestId")
	}
	r.mu.Lock()
	replies := r.pending[requestID]
	r.mu.Unlock()
	if replies == nil {
		return map[string]any{"ok": false, "stale": true}, nil
	}
	reply := remoteAgentRouteReply{result: params["result"]}
	if message := strings.TrimSpace(fieldString(params, "error")); message != "" {
		reply.err = errors.New(redactedSessionString(message))
	}
	select {
	case replies <- reply:
		return map[string]any{"ok": true}, nil
	default:
		return map[string]any{"ok": false, "stale": true}, nil
	}
}

func newRemoteAgentRouteID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "agent-route-" + hex.EncodeToString(raw), nil
}

// Never forward the caller's bearer capability to renderer JavaScript. The
// random route id authenticates the local response; the remote machine link
// applies its own device/controller authorization at the destination daemon.
func copyRemoteAgentRouteParams(params map[string]any) map[string]any {
	out := copyAnyMap(params)
	delete(out, "owner_key")
	delete(out, "parent_chat_id")
	delete(out, "parent_tab_id")
	return out
}

func splitMachineTaggedID(value string) (machineID, localID string, tagged bool, err error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, machineTaggedIDPrefix) {
		return "", value, false, nil
	}
	rest := strings.TrimPrefix(value, machineTaggedIDPrefix)
	cut := strings.IndexByte(rest, '~')
	if cut <= 0 || cut == len(rest)-1 {
		return "", "", false, errors.New("malformed machine-tagged chat id")
	}
	return rest[:cut], rest[cut+1:], true, nil
}

func remoteAgentChatTarget(params map[string]any) (string, bool, error) {
	tabMachine, _, tabTagged, tabErr := splitMachineTaggedID(fieldString(params, "tab_id"))
	if tabErr != nil {
		return "", false, tabErr
	}
	chatMachine, _, chatTagged, chatErr := splitMachineTaggedID(fieldString(params, "chat_id"))
	if chatErr != nil {
		return "", false, chatErr
	}
	if tabTagged != chatTagged {
		return "", false, errors.New("tab_id and chat_id must belong to the same machine")
	}
	if !tabTagged {
		return "", false, nil
	}
	if tabMachine != chatMachine {
		return "", false, errors.New("tab_id and chat_id belong to different machines")
	}
	return tabMachine, true, nil
}

func mergeRemoteAgentChatList(local map[string]any, remote any) map[string]any {
	remoteMap := mapFromAnyMain(remote)
	remoteChats := anySlice(remoteMap["chats"])
	if len(remoteChats) == 0 {
		return local
	}
	seen := make(map[string]struct{})
	merged := append([]any(nil), anySlice(local["chats"])...)
	for _, raw := range merged {
		chat := mapFromAnyMain(raw)
		seen[fieldString(chat, "tabId")+"\x00"+fieldString(chat, "chatId")] = struct{}{}
	}
	for _, raw := range remoteChats {
		chat := mapFromAnyMain(raw)
		machineID, tagged, err := remoteAgentChatTarget(map[string]any{
			"tab_id": fieldString(chat, "tabId"), "chat_id": fieldString(chat, "chatId"),
		})
		if err != nil || !tagged || machineID == "" {
			continue
		}
		key := fieldString(chat, "tabId") + "\x00" + fieldString(chat, "chatId")
		if _, exists := seen[key]; exists {
			continue
		}
		chat["machineId"] = machineID
		merged = append(merged, chat)
		seen[key] = struct{}{}
	}
	local["chats"] = merged
	return local
}
