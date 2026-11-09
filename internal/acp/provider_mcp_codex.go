package acp

import (
	"path/filepath"
	"regexp"
	"strings"
)

// codexMCPConfigStrategy reads the MCP servers in Codex's config.toml
// ([mcp_servers.NAME] tables, inline tables under [mcp_servers], or dotted
// mcp_servers.NAME.* keys) and whether each says enabled = false. It is a
// line reader for exactly that, not a TOML parser: it skips multi-line strings
// and arrays so nothing inside a value is mistaken for a table, and it never
// reads a value other than enabled.
type codexMCPConfigStrategy struct{}

func codexMCPConfigFile(scope mcpConfigScope) string {
	if dir := scope.Env("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "config.toml")
	}
	if scope.Home == "" {
		return ""
	}
	return filepath.Join(scope.Home, ".codex", "config.toml")
}

func (codexMCPConfigStrategy) ConfiguredServers(scope mcpConfigScope) ([]MCPServerEntry, error) {
	path := codexMCPConfigFile(scope)
	if path == "" {
		return nil, nil
	}
	data, exists, err := readMCPConfigFile(path)
	if err != nil || !exists {
		return nil, err
	}
	names, off := codexMCPServersFromTOML(string(data))
	servers := make([]MCPServerEntry, 0, len(names))
	for _, name := range names {
		entry := MCPServerEntry{Name: name, Scope: MCPScopeUser, File: path}
		if off[name] {
			entry.Status = MCPStatusOff
		}
		servers = append(servers, entry)
	}
	return servers, nil
}

// codexMCPServersFromTOML returns server names in file order and the ones
// switched off.
func codexMCPServersFromTOML(text string) ([]string, map[string]bool) {
	var names []string
	seen := map[string]bool{}
	off := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	// table is the current [header] as key parts.
	var table []string
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[[") {
			// An array of tables is never an MCP server.
			table = []string{"\x00"}
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := tomlClosingBracket(line)
			if end < 0 {
				table = []string{"\x00"}
				continue
			}
			table = tomlKeyParts(line[1:end])
			if len(table) >= 2 && table[0] == "mcp_servers" {
				add(table[1])
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		parts := append(append([]string(nil), table...), tomlKeyParts(key)...)
		value = strings.TrimSpace(value)
		// A value that opens a multi-line string, array or inline table runs
		// until it closes; gather it so its lines are not read as keys.
		value, index = tomlGatherValue(value, lines, index)
		if len(parts) < 2 || parts[0] != "mcp_servers" {
			continue
		}
		name := parts[1]
		add(name)
		switch {
		case len(parts) == 3 && parts[2] == "enabled":
			if tomlBareValue(value) == "false" {
				off[name] = true
			}
		case len(parts) == 2 && strings.HasPrefix(value, "{"):
			if tomlInlineEnabledFalse.MatchString(value) {
				off[name] = true
			}
		}
	}
	return names, off
}

var tomlInlineEnabledFalse = regexp.MustCompile(`(^|[{,\s])enabled\s*=\s*false\b`)

// tomlClosingBracket is the index of the ] that closes a [header], outside
// quotes.
func tomlClosingBracket(line string) int {
	quote := byte(0)
	for i := 1; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ']':
			return i
		}
	}
	return -1
}

// tomlKeyParts splits a dotted key, unquoting quoted parts.
func tomlKeyParts(key string) []string {
	var parts []string
	var current strings.Builder
	quote := byte(0)
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' && i+1 < len(key) {
				i++
				current.WriteByte(key[i])
			} else if c == quote {
				quote = 0
			} else {
				current.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '.':
			parts = append(parts, strings.TrimSpace(current.String()))
			current.Reset()
		case c == ' ' || c == '\t':
		default:
			current.WriteByte(c)
		}
	}
	return append(parts, strings.TrimSpace(current.String()))
}

// tomlGatherValue extends a value over following lines while a multi-line
// string, array or inline table it opened is still open.
func tomlGatherValue(value string, lines []string, index int) (string, int) {
	for _, delimiter := range []string{`"""`, `'''`} {
		if strings.HasPrefix(value, delimiter) {
			rest := value[len(delimiter):]
			for !strings.Contains(rest, delimiter) && index+1 < len(lines) {
				index++
				rest += "\n" + lines[index]
			}
			return delimiter + rest, index
		}
	}
	depth := tomlOpenDepth(value)
	for depth > 0 && index+1 < len(lines) {
		index++
		value += "\n" + lines[index]
		depth = tomlOpenDepth(value)
	}
	return value, index
}

// tomlOpenDepth counts [ and { still open outside strings and comments.
func tomlOpenDepth(value string) int {
	depth := 0
	quote := byte(0)
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			for i < len(value) && value[i] != '\n' {
				i++
			}
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		}
	}
	return depth
}

// tomlBareValue is a scalar without a trailing comment.
func tomlBareValue(value string) string {
	if before, _, found := strings.Cut(value, "#"); found {
		value = before
	}
	return strings.TrimSpace(value)
}
