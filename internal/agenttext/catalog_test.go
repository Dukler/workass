package agenttext

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Both complete key coverage and extraction are enforced at source boundaries:
// adding a missing key or embedding another prompt paragraph fails the gate.
func TestInstructionCatalogBoundaries(t *testing.T) {
	root := filepath.Join("..", "..")
	used := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fs := token.NewFileSet()
			file, err := parser.ParseFile(fs, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "agenttext" || sel.Sel.Name != "Get" {
					return true
				}
				if len(call.Args) != 1 {
					t.Error("invalid catalog access", path)
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok {
					t.Error("catalog keys must be static", path)
					return true
				}
				key, _ := strconv.Unquote(lit.Value)
				if _, ok := entries[key]; !ok {
					t.Error("missing catalog entry", key)
				}
				used[key] = true
				return true
			})
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				switch fn.Name.Name {
				case "agentMCPTools", "browserMCPTools", "buildEnvironmentBrief", "buildContextHistoryBlock", "buildUserRequestBlockWithToolRules", "nativeUserRequestBlock", "nativeChatPrompt", "buildTurnRuntimeIdentity", "promptBlocks":
					ast.Inspect(fn, func(n ast.Node) bool {
						lit, ok := n.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							return true
						}
						value, _ := strconv.Unquote(lit.Value)
						if len(strings.Fields(value)) >= 3 {
							t.Errorf("instruction prose outside catalog at %s: %q", fs.Position(lit.Pos()), value)
						}
						return true
					})
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range entries {
		if !used[key] {
			t.Error("unused catalog entry", key)
		}
		if strings.TrimSpace(value) == "" {
			t.Error("empty catalog entry", key)
		}
	}
}

// The guide is read once, on demand, so it is where new guidance goes. The other
// three are delivered with every session or turn, and this repo does not let them
// accumulate: each holds at its size from before this guidance was added.
func TestGuideSaysWhichToolForWhichJobWithinItsBudget(t *testing.T) {
	const guideBefore, guideBudgetBytes = 586, 800 // about 200 tokens
	guide := Get("native.guide")
	if len(guide) > guideBefore+guideBudgetBytes {
		t.Fatalf("native.guide is %d bytes, %d over its budget", len(guide), len(guide)-guideBefore-guideBudgetBytes)
	}
	for _, want := range []string{
		"workass_discover_machines", "workass_spawn_subagent", "workass_create_subsession", "workass_create_chat",
		"workass_fork_run", "`workass run`", "workass_start_process_child", "workass_register_external_work",
		"tools list --full",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("the guide does not mention %s", want)
		}
	}
	lower := strings.ToLower(guide)
	if !strings.Contains(lower, "native") {
		t.Error("the guide does not place the harness's own agents")
	}
	if strings.Contains(lower, "thinking") {
		t.Error("Workass tells agents nothing about thinking")
	}
	for key, before := range map[string]int{"environment.controls": 1077, "native.bootstrap": 677, "cli.bootstrap": 722} {
		if got := len(Get(key)); got > before {
			t.Errorf("%s grew from %d to %d bytes, and it is delivered with every session", key, before, got)
		}
	}
	if strings.Contains(Get("cli.bootstrap"), "complete catalog") {
		t.Error("cli.bootstrap still calls the short listing the complete catalog")
	}
}
