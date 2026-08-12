package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This is a build gate, not a cleanup preference. After actor cutover the old
// session mirror may be decoded only by provider_chat_migration.go. A future
// handler must not quietly restore one of its semantic mutation/read paths as
// a compatibility fallback.
func TestProductionDoesNotCallLegacySessionSemanticAuthority(t *testing.T) {
	banned := map[string]struct{}{
		"Save": {}, "GetWithLiveSessions": {}, "GetRawWithLiveSessions": {},
		"UpdateChatControls": {}, "ChatRuntimeControls": {}, "UpdateChatWorkspace": {}, "MoveChatWorkspace": {}, "ChatWorkspace": {},
		"MostRecentVisibleAssistantJobID": {}, "PrepareTurn": {}, "FailPreparedTurn": {}, "PreparedTurnPublicFields": {},
		"QueueRendererStartCollision": {}, "AdoptedQueueReceipt": {}, "RecordJobEvent": {}, "recordJobEvent": {},
		"BeginLiveSteer": {}, "AcknowledgeLiveSteer": {}, "CommitLiveSteerBoundary": {}, "RejectLiveSteer": {},
		"AgentChatList": {}, "AgentReadChat": {}, "AgentCreateChat": {}, "AgentRenameChat": {}, "AgentConfigureChat": {},
		"AgentFocusChat": {}, "AgentDeleteChat": {}, "AgentEnqueueChat": {}, "AgentQueueHead": {}, "AgentAdoptRendererQueueHead": {},
		"AgentParkQueuedTurn": {}, "AgentQueueTargets": {}, "AgentPrepareQueuedTurn": {}, "AgentCommitLiveSteer": {},
	}
	// These are the only sessionStore methods that may survive the cutover.
	// They are either one-time migration/import entry points or daemon-global /
	// content-addressed attachment plumbing.  Keep this allow-list explicit so
	// adding a method to the retired semantic surface cannot accidentally pass
	// because it happens to live in session_store.go.
	allowed := map[string]struct{}{
		"Get": {}, "GlobalSnapshot": {}, "ActivateActorCutover": {},
		"SaveActorGlobalSnapshot": {}, "SaveGlobalActiveTab": {},
		"PersistProviderAttachments": {}, "ResolveProviderAttachment": {},
		"LoadError": {}, "enabled": {},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "provider_chat_migration.go" {
			continue
		}
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if declaration, ok := node.(*ast.FuncDecl); ok && declaration.Recv != nil && len(declaration.Recv.List) == 1 {
				methodName := declaration.Name.Name
				if _, forbidden := banned[methodName]; forbidden {
					if _, explicitlyAllowed := allowed[methodName]; !explicitlyAllowed && isSessionStoreReceiver(declaration.Recv.List[0].Type) {
						position := set.Position(declaration.Pos())
						violations = append(violations, position.String()+" declares "+methodName)
					}
				}
				return true
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, forbidden := banned[selector.Sel.Name]; !forbidden {
				return true
			}
			if !legacySessionReceiver(selector.X) {
				return true
			}
			position := set.Position(call.Pos())
			violations = append(violations, position.String()+" calls "+selector.Sel.Name)
			return true
		})
	}
	if len(violations) != 0 {
		sort.Strings(violations)
		t.Fatalf("legacy session semantic authority escaped its one-time migration boundary:\n%s", strings.Join(violations, "\n"))
	}
}

func isSessionStoreReceiver(expression ast.Expr) bool {
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == "sessionStore"
}

func legacySessionReceiver(expression ast.Expr) bool {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name == "sessionState" || value.Name == "store" || value.Name == "sessions"
	case *ast.SelectorExpr:
		owner, ok := value.X.(*ast.Ident)
		return ok && owner.Name == "r" && value.Sel.Name == "sessions"
	default:
		return false
	}
}
