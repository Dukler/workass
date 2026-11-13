package chat

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"workass/internal/provider"
)

// A static proof that complements the runtime check in TestMain: no function
// reachable from a tool-sharing command's reducer writes a tool-owner map. A
// write is an assignment, increment, delete or clear whose target goes through
// a .Tools selector, or taking its address. Method calls are resolved by name
// to every method of that name, so the reach is an over-approximation.
func TestToolLeavingCommandsLeaveTheTools(t *testing.T) {
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
	roots := []struct {
		command Command
		reducer string
	}{
		{CommitLaneSelection{}, "reduceCommitLaneSelection"},
		{Submit{}, "reduceSubmit"},
		{ClaimEffect{}, "reduceClaimEffect"},
		{TurnAdmitted{Accepted: true}, "reduceTurnAccepted"},
	}
	for _, root := range roots {
		if !commandLeavesTools(root.command) {
			t.Errorf("%T is checked here but not shared by commandLeavesTools", root.command)
		}
	}
	for _, command := range []Command{TurnAdmitted{}, TurnAdmitted{Accepted: true, Ambiguous: true}, TurnAdmitted{Ambiguous: true}, ProviderEventReceived{}, TurnTerminated{}, CancelTurn{}, RecoverOutbox{}} {
		if commandLeavesTools(command) {
			t.Errorf("%#v may write tool owners but is shared by commandLeavesTools", command)
		}
	}
	reach := func(root string) map[string]bool {
		seen := map[string]bool{}
		queue := []string{root, "recordEffects", "validateTransition"}
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
	throughTools := func(expr ast.Expr) bool {
		found := false
		ast.Inspect(expr, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "Tools" {
				found = true
			}
			return !found
		})
		return found
	}
	// These build and return a new State; their only tool writes set the
	// copy's own field.
	constructsState := map[string]bool{"Clone": true, "cloneSharingLedger": true, "cloneSharing": true, "cloneLedgerRows": true, "cloneLedgerRowsSharing": true}
	for _, root := range roots {
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
					case *ast.CallExpr:
						if ident, ok := node.Fun.(*ast.Ident); ok && (ident.Name == "delete" || ident.Name == "clear") && len(node.Args) > 0 {
							targets = []ast.Expr{node.Args[0]}
						}
					}
					for _, target := range targets {
						if throughTools(target) {
							writes = append(writes, fmt.Sprintf("%s (%s)", fset.Position(target.Pos()), name))
						}
					}
					return true
				})
			}
		}
		sort.Strings(writes)
		if len(writes) > 0 {
			t.Errorf("%T shares the tool owners but reaches tool writes:\n  %s", root.command, strings.Join(writes, "\n  "))
		}
	}
}

// validateTransition must decide exactly what Validate decides. Each case
// breaks one input a tool owner's check reads and expects the transition's
// shortcut to notice it.
func TestValidateTransitionRechecksWhatAToolOwnerReads(t *testing.T) {
	current, owner := providerActivityHistoricalState(t, "turn-1")
	lane := owner.LaneID
	generation := current.Lanes[lane].ConnectionGeneration
	current.Tools["tool-1"] = ToolState{Owner: owner}
	if err := current.Validate(); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	transition := func(edit func(*State)) State {
		next := current.cloneSharing(true)
		next.Ledger = append([]LedgerEvent(nil), current.Ledger...)
		edit(&next)
		return next
	}
	cases := map[string]func(*State){
		"unchanged":            func(*State) {},
		"lane removed":         func(next *State) { delete(next.Lanes, lane) },
		"generation lowered":   func(next *State) { setGeneration(next, lane, generation-1) },
		"generation raised":    func(next *State) { setGeneration(next, lane, generation+1) },
		"ledger turn changed":  func(next *State) { next.Ledger[0].NativeTurnID = "turn-2" },
		"ledger row excluded":  func(next *State) { next.Ledger[0].ContextExcluded = true },
		"ledger row inherited": func(next *State) { next.Ledger[0].Inherited = true },
		"ledger row removed":   func(next *State) { next.Ledger = nil },
		"ledger row loses its event id": func(next *State) { next.Ledger[0].EventID = "" },
		"foreground takes the owner's turn with another native id": func(next *State) {
			next.Foreground = &ForegroundTurn{LaneID: lane, OperationID: owner.OperationID, Turn: provider.TurnRef{NativeID: "turn-9"}}
		},
		"foreground takes the owner's turn with its native id": func(next *State) {
			next.Foreground = &ForegroundTurn{LaneID: lane, OperationID: owner.OperationID, Turn: provider.TurnRef{NativeID: "turn-1"}}
		},
		"foreground on another operation": func(next *State) {
			next.Foreground = &ForegroundTurn{LaneID: lane, OperationID: "op-2"}
		},
	}
	mustFail := map[string]bool{"lane removed": true, "ledger row loses its event id": true, "generation lowered": true, "ledger turn changed": true, "ledger row removed": true,
		"foreground takes the owner's turn with another native id": true}
	for name, edit := range cases {
		next := transition(edit)
		want := next.Validate()
		if mustFail[name] && want == nil {
			t.Fatalf("%s: the fixture no longer breaks the tool owner", name)
		}
		// Only the tool owners are compared: the rest of a hand-made
		// foreground may be invalid on its own, and both paths check it.
		got := validateTransition(current, next, true, true)
		if (want == nil) != (got == nil) || (want != nil && want.Error() != got.Error()) {
			t.Errorf("%s: validateTransition = %v, Validate = %v", name, got, want)
		}
	}
	// The foreground keeps its key and changes its native turn: the owners
	// it answers for are checked again.
	running, activeOwner := providerActivityActiveState(t)
	running.Tools["tool-active"] = ToolState{Owner: activeOwner}
	if err := running.Validate(); err != nil {
		t.Fatalf("running fixture: %v", err)
	}
	moved := running.cloneSharing(true)
	foreground := *running.Foreground
	foreground.Turn.NativeID = "another-native-turn"
	moved.Foreground = &foreground
	want, got := moved.Validate(), validateTransition(running, moved, true, true)
	if want == nil || got == nil || want.Error() != got.Error() {
		t.Fatalf("foreground native turn changed: validateTransition = %v, Validate = %v", got, want)
	}
	// An unshared map, or a current state nobody validated, is a full check.
	next := current.cloneSharing(false)
	setGeneration(&next, lane, generation-1)
	if validateTransition(current, next, true, true) == nil {
		t.Error("a copied tool map skipped the tool owners")
	}
	shared := current.cloneSharing(true)
	setGeneration(&shared, lane, generation-1)
	if validateTransition(current, shared, false, true) == nil {
		t.Error("an unvalidated current state skipped the tool owners")
	}
}

func setGeneration(state *State, laneID provider.LaneID, generation uint64) {
	lane := state.Lanes[laneID]
	lane.ConnectionGeneration = generation
	state.Lanes[laneID] = lane
}
