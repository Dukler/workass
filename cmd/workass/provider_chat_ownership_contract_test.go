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

// This build gate keeps chat semantics out of daemon-global presentation
// storage. A future handler must not restore a second chat mutation/read path.
func TestProductionGlobalSessionStoreHasNoChatSemanticAuthority(t *testing.T) {
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
	// These are the only global-presentation/content-addressed attachment store
	// methods. Keep this list explicit so chat semantics cannot drift back in.
	allowed := map[string]struct{}{
		"Get": {}, "GlobalSnapshot": {},
		"SaveActorGlobalSnapshot": {}, "SaveGlobalActiveTab": {},
		"PersistProviderAttachments": {}, "ResolveProviderAttachment": {},
		"PlanProviderAttachments": {},
		"LoadError":               {}, "enabled": {},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
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
			if !globalSessionReceiver(selector.X) {
				return true
			}
			position := set.Position(call.Pos())
			violations = append(violations, position.String()+" calls "+selector.Sel.Name)
			return true
		})
	}
	if len(violations) != 0 {
		sort.Strings(violations)
		t.Fatalf("chat semantic authority escaped into global session storage:\n%s", strings.Join(violations, "\n"))
	}
}

func TestSessionStoreRemovedRuntimeSurfaceIsPhysicallyAbsent(t *testing.T) {
	raw, err := os.ReadFile("session_store.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	forbidden := []string{
		"type sessionJob struct",
		"jobs           map[string]*sessionJob",
		"pending        []*sessionJob",
		"jobOrder       []string",
		"recoveredSessionJournal",
		"sessionJournalQuarantineError",
		"replaySessionJournalsLocked",
		"applySessionJournalLocked",
		"beginLiveSteerLocked",
		"stageLiveSteerLocked",
		"commitStagedSteerLocked",
		"rejectLiveSteerLocked",
		"markSteerConsumedLocked",
		"settleJobSteersLocked",
		"interruptOrphanedTurnsLocked",
		"writeLocked",
		"stageSnapshotBytesLocked",
	}
	for _, token := range forbidden {
		if strings.Contains(source, token) {
			t.Errorf("removed pre-actor session-store symbol remains: %q", token)
		}
	}
}

func TestObsoleteSpawnedWorkConversionSurfaceIsPhysicallyAbsent(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		source := string(raw)
		for _, token := range []string{
			"SpawnedWorkPairsForMigration(",
			"PruneSpawnedWorkForMigration(",
		} {
			if strings.Contains(source, token) {
				t.Errorf("obsolete spawned-work conversion surface remains in %q: %s", name, token)
			}
		}
	}
}

func isSessionStoreReceiver(expression ast.Expr) bool {
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == "sessionStore"
}

func globalSessionReceiver(expression ast.Expr) bool {
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
