package acp

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestProviderPrivateTokensStayAtRegisteredBoundaries protects the architectural
// direction, not a spelling convention in one file. Go is parsed as Go, so
// comments and formatting cannot create false positives. Renderer and shell
// sources use a small comment-aware lexer because the repository deliberately
// ships no JavaScript parser dependency.
func TestProviderPrivateTokensStayAtRegisteredBoundaries(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	violations := append(
		scanGoProviderBoundaries(t, root),
		scanScriptProviderBoundaries(t, root)...,
	)
	sort.Strings(violations)
	for _, violation := range violations {
		t.Error(violation)
	}
}

func scanGoProviderBoundaries(t *testing.T, root string) []string {
	t.Helper()
	var violations []string
	files := productionFiles(t, root, []string{"cmd", "internal"}, func(path string) bool {
		return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
	})
	for _, path := range files {
		rel := slashRelative(t, root, path)
		if isRegisteredGoProviderBoundary(rel) {
			continue
		}
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, nil, 0)
		if err != nil {
			t.Fatalf("parse production Go source %s: %v", rel, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.Ident:
				if reason := forbiddenGoProviderIdentifier(value.Name); reason != "" {
					position := set.Position(value.Pos())
					violations = append(violations, providerBoundaryViolation(rel, position.Line, value.Name, reason))
				}
			case *ast.BasicLit:
				if value.Kind != token.STRING {
					break
				}
				literal, err := strconv.Unquote(value.Value)
				if err != nil {
					position := set.Position(value.Pos())
					violations = append(violations, fmt.Sprintf("%s:%d cannot decode string literal: %v", rel, position.Line, err))
					break
				}
				if reason := forbiddenProviderLiteral(literal); reason != "" {
					position := set.Position(value.Pos())
					violations = append(violations, providerBoundaryViolation(rel, position.Line, literal, reason))
				}
			}
			return true
		})
	}
	return violations
}

func scanScriptProviderBoundaries(t *testing.T, root string) []string {
	t.Helper()
	var violations []string
	files := productionFiles(t, root, []string{"desktop/renderer2/src", "desktop/shell"}, func(path string) bool {
		base := filepath.Base(path)
		if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
			return false
		}
		switch strings.ToLower(filepath.Ext(base)) {
		case ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx":
			return true
		default:
			return false
		}
	})
	for _, path := range files {
		rel := slashRelative(t, root, path)
		if rel == "desktop/renderer2/src/icons.tsx" {
			// This file is the one presentation-only boundary allowed to map an
			// adapter-authored AssistantBrand to its artwork.
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read production script %s: %v", rel, err)
		}
		for _, sourceToken := range lexScriptSource(raw) {
			var reason string
			switch sourceToken.kind {
			case scriptStringToken:
				reason = forbiddenProviderLiteral(sourceToken.value)
			case scriptIdentifierToken:
				reason = forbiddenScriptProviderIdentifier(sourceToken.value)
			}
			if reason != "" {
				violations = append(violations, providerBoundaryViolation(rel, sourceToken.line, sourceToken.value, reason))
			}
		}
	}
	return violations
}

func productionFiles(t *testing.T, root string, directories []string, keep func(string) bool) []string {
	t.Helper()
	var files []string
	for _, directory := range directories {
		base := filepath.Join(root, filepath.FromSlash(directory))
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				switch entry.Name() {
				case "node_modules", "dist", "out":
					if path != base {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if keep(path) {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk production sources under %s: %v", directory, err)
		}
	}
	sort.Strings(files)
	return files
}

func slashRelative(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("resolve source path %s: %v", path, err)
	}
	return filepath.ToSlash(rel)
}

func isRegisteredGoProviderBoundary(rel string) bool {
	if !strings.HasPrefix(rel, "internal/acp/") {
		return false
	}
	base := filepath.Base(rel)
	if base == "provider_registration.go" || base == "provider.go" {
		return true
	}
	for _, sharedAdapter := range []string{
		"provider_delivery.go",
		"provider_plan_usage.go",
		"provider_catalog_strategy.go",
		"provider_permission.go",
	} {
		if base == sharedAdapter {
			return true
		}
	}
	for _, providerID := range providerBoundaryIDs {
		if strings.HasPrefix(base, "provider_") && strings.HasSuffix(base, "_"+providerID+".go") {
			return true
		}
		if base == "native_"+providerID+".go" || base == providerID+"_update.go" {
			return true
		}
	}
	return false
}

var providerBoundaryIDs = []string{"claude", "codex", "devin", "qwen"}

var providerPrivateMarkers = []string{
	"_workass_claude",
	"_workass_codex",
	"_workass/claude",
	"_workass/codex",
	"workass.claude",
	"workass.codex",
	"claudecode",
	"parenttooluseid",
	"run_in_background",
	"claude_code_disable_auto_memory",
}

func forbiddenProviderLiteral(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	for _, providerID := range providerBoundaryIDs {
		if value == providerID {
			return "provider ID literal belongs in registration, an adapter, a native host, or presentation icons"
		}
	}
	for _, marker := range providerPrivateMarkers {
		if strings.Contains(value, marker) {
			return "provider-private wire key belongs in a registered adapter"
		}
	}
	return ""
}

func forbiddenGoProviderIdentifier(identifier string) string {
	lower := strings.ToLower(identifier)
	for _, providerID := range providerBoundaryIDs {
		if strings.Contains(lower, providerID) {
			return "provider-named behavior belongs in registration or an adapter"
		}
	}
	compactIdentifier := compactProviderBoundaryToken(lower)
	for _, marker := range providerPrivateMarkers {
		compact := compactProviderBoundaryToken(marker)
		if compact != "" && strings.Contains(compactIdentifier, compact) {
			return "provider-private wire identifier belongs in a registered adapter"
		}
	}
	return ""
}

func forbiddenScriptProviderIdentifier(identifier string) string {
	// Executable provider ids are canonical lowercase. Keeping this
	// case-sensitive avoids mistaking JSX prose such as "Claude Code" for a
	// branch while still rejecting providerId, shellStatus.claude, and variables
	// such as claudeSession outside an allowed boundary.
	for _, providerID := range providerBoundaryIDs {
		if strings.Contains(identifier, providerID) {
			return "provider-named renderer/shell behavior belongs behind daemon-authored neutral data"
		}
	}
	lower := strings.ToLower(identifier)
	compactIdentifier := compactProviderBoundaryToken(lower)
	for _, marker := range providerPrivateMarkers {
		compact := compactProviderBoundaryToken(marker)
		if compact != "" && strings.Contains(compactIdentifier, compact) {
			return "provider-private renderer/shell identifier is forbidden"
		}
	}
	return ""
}

func compactProviderBoundaryToken(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '/', '.', ':':
			return -1
		default:
			return r
		}
	}, strings.ToLower(value))
}

func providerBoundaryViolation(path string, line int, value, reason string) string {
	value = strings.ReplaceAll(value, "\n", `\n`)
	if len(value) > 100 {
		value = value[:100] + "..."
	}
	return fmt.Sprintf("%s:%d %s: %q", path, line, reason, value)
}

type scriptTokenKind uint8

const (
	scriptIdentifierToken scriptTokenKind = iota + 1
	scriptStringToken
)

type scriptToken struct {
	kind  scriptTokenKind
	value string
	line  int
}

func lexScriptSource(source []byte) []scriptToken {
	line := 1
	tokens := make([]scriptToken, 0, len(source)/24)
	for offset := 0; offset < len(source); {
		current := source[offset]
		if current == '\n' {
			line++
			offset++
			continue
		}
		if current == '/' && offset+1 < len(source) && source[offset+1] == '/' {
			offset += 2
			for offset < len(source) && source[offset] != '\n' {
				offset++
			}
			continue
		}
		if current == '/' && offset+1 < len(source) && source[offset+1] == '*' {
			offset += 2
			for offset+1 < len(source) && !(source[offset] == '*' && source[offset+1] == '/') {
				if source[offset] == '\n' {
					line++
				}
				offset++
			}
			if offset+1 < len(source) {
				offset += 2
			}
			continue
		}
		if current == '\'' || current == '"' || current == '`' {
			quote, tokenLine := current, line
			offset++
			start := offset
			var value strings.Builder
			for offset < len(source) {
				if source[offset] == '\\' && offset+1 < len(source) {
					value.Write(source[start:offset])
					value.WriteByte(source[offset+1])
					offset += 2
					start = offset
					continue
				}
				if source[offset] == quote {
					value.Write(source[start:offset])
					offset++
					break
				}
				if source[offset] == '\n' {
					line++
				}
				offset++
			}
			if start < offset && (offset == len(source) || source[offset-1] != quote) {
				value.Write(source[start:offset])
			}
			tokens = append(tokens, scriptToken{kind: scriptStringToken, value: value.String(), line: tokenLine})
			continue
		}
		if isScriptIdentifierStart(current) {
			start, tokenLine := offset, line
			offset++
			for offset < len(source) && isScriptIdentifierPart(source[offset]) {
				offset++
			}
			tokens = append(tokens, scriptToken{kind: scriptIdentifierToken, value: string(source[start:offset]), line: tokenLine})
			continue
		}
		offset++
	}
	return tokens
}

func isScriptIdentifierStart(value byte) bool {
	return value == '_' || value == '$' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func isScriptIdentifierPart(value byte) bool {
	return isScriptIdentifierStart(value) || value >= '0' && value <= '9'
}
