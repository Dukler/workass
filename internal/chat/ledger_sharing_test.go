package chat

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Every test in this package runs with the ledger-sharing check on: a
// transition that shares the committed ledger rows (commandLeavesLedger) must
// leave them exactly as they were, in the committed state and in its result.
func TestMain(m *testing.M) {
	checkLedgerSharing = func(command Command, before, current []LedgerEvent, next *State) {
		if !sameLedgerRows(before, current) || !sameLedgerRows(before, next.Ledger) {
			panic(fmt.Sprintf("%T shares the ledger but changed it", command))
		}
	}
	os.Exit(m.Run())
}

func sameLedgerRows(left, right []LedgerEvent) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !reflect.DeepEqual(left[index], right[index]) {
			return false
		}
	}
	return true
}

// A static proof that complements the runtime check above: no function
// reachable from a ledger-sharing command's reducer writes the ledger. A
// write is an assignment, increment or append whose target goes through a
// .Ledger selector, or taking the address of a ledger row. Method calls are
// resolved by name to every method of that name, so the reach is an
// over-approximation.
func TestLedgerLeavingCommandsLeaveTheLedger(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string][]*ast.FuncDecl{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				funcs[fn.Name.Name] = append(funcs[fn.Name.Name], fn)
			}
		}
	}
	// Each sharing command and the reducer Reduce dispatches it to; Reduce
	// itself then records effects and validates.
	roots := []struct {
		command Command
		reducer string
	}{
		{UpdatePresentation{}, "reduceUpdatePresentation"}, {SavePresentation{}, "reduceSavePresentation"},
		{UpdateEnvironment{}, "reduceUpdateEnvironment"}, {ChangeWorkspace{}, "reduceChangeWorkspace"},
		{ReplaceStagedQueue{}, "reduceReplaceStagedQueue"}, {PromoteStagedQueue{}, "reducePromoteStagedQueue"},
		{UpdateRuntimeControls{}, "reduceUpdateRuntimeControls"}, {SaveRuntimeControls{}, "reduceSaveRuntimeControls"},
		{AttachTab{}, "reduceAttachTab"}, {Submit{}, "reduceSubmit"}, {Steer{}, "reduceSteer"},
		{ResolvePermission{}, "reduceResolvePermission"}, {PermissionDecided{}, "reducePermissionDecided"},
		{RecordCancelReceipt{}, "reduceRecordCancelReceipt"}, {RecordAgentWaitObservation{}, "reduceRecordAgentWaitObservation"},
		{RecordExternalMutation{}, "reduceRecordExternalMutation"}, {ExternalMutationReceipt{}, "reduceExternalMutationReceipt"},
		{ClaimEffect{}, "reduceClaimEffect"},
	}
	checked := map[string]bool{}
	for _, root := range roots {
		checked[reflect.TypeOf(root.command).Name()] = true
		if !commandLeavesLedger(root.command) {
			t.Errorf("%T is checked here but not shared by commandLeavesLedger", root.command)
		}
	}
	// Every command commandLeavesLedger names must be one checked here.
	for _, fn := range funcs["commandLeavesLedger"] {
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if clause, ok := node.(*ast.CaseClause); ok {
				for _, expr := range clause.List {
					if ident, ok := expr.(*ast.Ident); ok && !checked[ident.Name] {
						t.Errorf("commandLeavesLedger shares %s, which this test does not check", ident.Name)
					}
				}
			}
			return true
		})
	}
	for _, command := range []Command{InitializeChat{}, InitializeFork{}, TurnAdmitted{}, InputConsumed{}, TurnTerminated{}, ProviderEventReceived{}, RecoverOutbox{}, SelectLane{}, BindEstablishedLane{}, CheckpointRestored{}} {
		if commandLeavesLedger(command) {
			t.Errorf("%T writes the ledger but is shared by commandLeavesLedger", command)
		}
	}
	reach := func(root string) map[string]bool {
		seen := map[string]bool{}
		queue := []string{root, "recordEffects", "Validate"}
		for len(queue) > 0 {
			name := queue[0]
			queue = queue[1:]
			if seen[name] {
				continue
			}
			seen[name] = true
			for _, fn := range funcs[name] {
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					switch node := node.(type) {
					case *ast.CallExpr:
						switch fun := node.Fun.(type) {
						case *ast.Ident:
							queue = append(queue, fun.Name)
						case *ast.SelectorExpr:
							queue = append(queue, fun.Sel.Name)
						}
					case *ast.Ident:
						if _, ok := funcs[node.Name]; ok {
							queue = append(queue, node.Name)
						}
					}
					return true
				})
			}
		}
		return seen
	}
	throughLedger := func(expr ast.Expr) bool {
		found := false
		ast.Inspect(expr, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "Ledger" {
				found = true
			}
			return !found
		})
		return found
	}
	// These build and return a new State; their only ledger writes set the
	// copy's own field.
	constructsState := map[string]bool{"Clone": true, "cloneSharingLedger": true}
	for _, root := range roots {
		command := root.command
		if len(funcs[root.reducer]) == 0 {
			t.Fatalf("%T: reducer %s does not exist", command, root.reducer)
		}
		var writes []string
		for name := range reach(root.reducer) {
			if constructsState[name] {
				continue
			}
			for _, fn := range funcs[name] {
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					var targets []ast.Expr
					switch node := node.(type) {
					case *ast.AssignStmt:
						targets = node.Lhs
					case *ast.IncDecStmt:
						targets = []ast.Expr{node.X}
					case *ast.UnaryExpr:
						if node.Op == token.AND {
							targets = []ast.Expr{node.X}
						}
					}
					for _, target := range targets {
						if throughLedger(target) {
							writes = append(writes, fmt.Sprintf("%s (%s)", fset.Position(target.Pos()), name))
						}
					}
					return true
				})
			}
		}
		sort.Strings(writes)
		if len(writes) > 0 {
			t.Errorf("%T shares the ledger but reaches ledger writes:\n  %s", command, strings.Join(writes, "\n  "))
		}
	}
}
