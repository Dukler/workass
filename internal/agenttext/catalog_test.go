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
