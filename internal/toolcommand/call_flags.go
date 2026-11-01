package toolcommand

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"workass/internal/acp"
	"workass/internal/toolcli"
)

// Usage is the one usage text. scripts/workass-tools.mjs carries the same string
// (TestUsageNeverAdvertisesContextAndMatchesTheWindowsClient holds them equal).
// It does not name --context: that flag still works for a controlled caller, but
// the guide forbids it to agents, so it is not advertised to them here.
const Usage = "workass tools guide\nworkass tools list [NAME | --full]\nworkass tools call NAME [--input FILE]\nlist shows each tool in one line, list NAME its schema, list --full every schema.\nCall arguments are one JSON object on stdin, or --input FILE, or inline JSON. Simple values also work as --key value. Mutations require operation_id; retry only with the same id and arguments."

// callArguments is where one `tools call` takes its arguments from.
type callArguments struct {
	input  string
	inline string
	pairs  map[string]string
	help   bool
}

var flagNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// parseCallFlags reads what follows `tools call NAME`. --input reads as it always
// did; anything else that looks like a flag is one argument for the daemon to
// type against the tool's schema. scripts/workass-tools.mjs parses the same way,
// held to it by scripts/tests/fixtures/tools-call-flags.json.
func parseCallFlags(tokens []string) (callArguments, error) {
	parsed := callArguments{pairs: map[string]string{}}
	var positional []string
	hasInput := false
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token == "--" {
			positional = append(positional, tokens[index+1:]...)
			break
		}
		if !strings.HasPrefix(token, "-") || token == "-" {
			positional = append(positional, token)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(token, "-"), "-"), "=")
		if name == "h" || name == "help" {
			parsed.help = true
			return parsed, nil
		}
		if !flagNamePattern.MatchString(name) {
			return parsed, errors.New("that is not a valid flag")
		}
		key := strings.ReplaceAll(name, "-", "_")
		switch key {
		case "context":
			return parsed, errors.New("--context is not needed: the process environment already names the context")
		case "input":
			if !hasValue {
				if index+1 >= len(tokens) {
					return parsed, errors.New("--input needs an argument")
				}
				index++
				value = tokens[index]
			}
			parsed.input, hasInput = value, true
			continue
		}
		if !hasValue {
			value = "true"
			if index+1 < len(tokens) && !looksLikeFlag(tokens[index+1]) {
				index++
				value = tokens[index]
			}
		}
		if _, repeated := parsed.pairs[key]; repeated {
			return parsed, fmt.Errorf("--%s was passed more than once", key)
		}
		parsed.pairs[key] = value
	}
	switch {
	case len(positional) > 1:
		return parsed, errors.New("unexpected call argument")
	case len(positional) == 1 && !looksLikeInlineJSON(positional[0]):
		return parsed, errors.New("unexpected call argument")
	case len(positional) == 1:
		parsed.inline = positional[0]
	}
	sources := 0
	for _, used := range []bool{hasInput, parsed.inline != "", len(parsed.pairs) > 0} {
		if used {
			sources++
		}
	}
	if sources > 1 {
		return parsed, errors.New("use one of --input, inline JSON or --key value, not both")
	}
	return parsed, nil
}

// looksLikeFlag says whether a token after a flag is the next flag rather than
// that flag's value. A negative number is a value: timeout_ms takes -1.
func looksLikeFlag(token string) bool {
	if strings.HasPrefix(token, "--") {
		return true
	}
	if len(token) < 2 || token[0] != '-' {
		return false
	}
	next := token[1]
	return next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z'
}

func looksLikeInlineJSON(value string) bool {
	return strings.HasPrefix(strings.TrimLeft(value, " \t\r\n"), "{")
}

// callSyntaxHelp is what a mistake says when there is no daemon to ask for that
// tool's own example.
func callSyntaxHelp(name string) string {
	return "pass arguments as one JSON object: on stdin, with --input FILE, or inline as tools call " + name + ` --input '{"key":"value"}'; tools list ` + name + " shows its arguments"
}

// explainCallSyntax turns a command line that could not be read into an error
// carrying a working example for that tool, which only the daemon can write. It
// is the one request this run makes, and nothing about the call has been sent.
func explainCallSyntax(ctx context.Context, contextFile, name string, mistake error) error {
	help := callSyntaxHelp(name)
	if config, err := readContext(contextFile); err == nil {
		query := url.Values{"name": {name}, "usage": {"1"}}
		if response, err := toolcli.DoQuery(ctx, config, query, nil); err == nil && response.Error == "" {
			if fields, ok := response.Result.(map[string]any); ok {
				if usage, ok := fields["usage"].(string); ok && usage != "" {
					help = usage
				}
			}
		}
	}
	return errors.New(acp.RedactSensitiveText(mistake.Error() + ". " + help))
}
