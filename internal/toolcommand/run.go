// Package toolcommand contains the short-lived authenticated Workass tools
// client shared by the daemon's early tools entrypoint and compatibility helper.
package toolcommand

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"workass/internal/acp"
	"workass/internal/toolcli"
)

// readContext reads this process's private context, saying what actually
// happened to it without ever repeating its path.
func readContext(contextFile string) (toolcli.Config, error) {
	if strings.TrimSpace(contextFile) == "" {
		return toolcli.Config{}, errors.New("WORKASS_TOOL_CONTEXT is not set; this process was not launched by a Workass chat engine")
	}
	config, err := toolcli.ReadConfig(contextFile)
	if err != nil {
		if errors.Is(err, toolcli.ErrContextMissing) {
			return toolcli.Config{}, fmt.Errorf("%w; the chat that launched this process was deleted, or the daemon cleaned up a context left over from before a restart; long-running helpers should not poll — ask the chat to use workass_schedule_message", err)
		}
		return toolcli.Config{}, fmt.Errorf("%w; use the current process WORKASS_TOOL_CONTEXT, not a context path copied from conversation history", err)
	}
	return config, nil
}

// Run executes one direct Workass CLI command. It performs exactly one
// authenticated request; uncertain mutations must be read back by the caller
// with the same operation id rather than retried here.
func Run(ctx context.Context, args []string, input io.Reader, output, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("workass tools", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	contextFile := flags.String("context", os.Getenv("WORKASS_TOOL_CONTEXT"), "private context file supplied by the current Workass turn")
	flags.Usage = func() {
		fmt.Fprintln(diagnostics, Usage)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		flags.Usage()
		return errors.New("a tools command is required")
	}
	var call *toolcli.Call
	name := ""
	query := url.Values{}
	switch args[0] {
	case "guide":
		guide, err := os.ReadFile(os.Getenv("WORKASS_TOOLS_GUIDE"))
		if err != nil {
			return errors.New("Workass guide is unavailable in this process")
		}
		if len(args) > 1 {
			// The guide is what was asked for, whatever else was typed after it.
			fmt.Fprintln(diagnostics, "guide takes no arguments; tools list NAME shows one tool's schema")
		}
		_, err = output.Write(guide)
		return err
	case "list":
		for _, argument := range args[1:] {
			switch {
			case argument == "--full" || argument == "-full":
				query.Set("full", "1")
			case strings.HasPrefix(argument, "-"):
				return errors.New("list accepts one exact tool name or --full")
			case name != "":
				return errors.New("list accepts at most one exact tool name")
			default:
				name = argument
			}
		}
		if name != "" {
			query = url.Values{"name": {name}}
		}
	case "call":
		if len(args) < 2 {
			return errors.New("call requires an exact tool name")
		}
		name = args[1]
		if strings.HasPrefix(name, "-") {
			return errors.New("call takes the exact tool name first: tools call NAME --key value")
		}
		parsed, syntaxErr := parseCallFlags(args[2:])
		if syntaxErr != nil {
			return explainCallSyntax(ctx, *contextFile, name, syntaxErr)
		}
		if parsed.help {
			fmt.Fprintln(diagnostics, Usage)
			return nil
		}
		if len(parsed.pairs) > 0 {
			arguments := make(map[string]any, len(parsed.pairs))
			for key, value := range parsed.pairs {
				arguments[key] = value
			}
			call = &toolcli.Call{Name: name, Arguments: arguments, FromFlags: true}
			break
		}
		var raw []byte
		if parsed.inline != "" {
			raw = []byte(parsed.inline)
		} else {
			if parsed.input != "" {
				f, err := os.Open(parsed.input)
				switch {
				case err == nil:
					defer f.Close()
					input = f
				case looksLikeInlineJSON(parsed.input):
					raw = []byte(parsed.input)
				case parsed.input == "-":
					// stdin, as most tools spell it.
				default:
					return errors.New("cannot open tool arguments file; --input takes a file path or one inline JSON object, and stdin works too")
				}
			}
			if raw == nil {
				read, err := io.ReadAll(io.LimitReader(input, 4*1024*1024+1))
				if err != nil {
					return errors.New("tool arguments are unreadable or exceed 4 MiB")
				}
				raw = read
			}
		}
		if len(raw) > 4*1024*1024 {
			return errors.New("tool arguments are unreadable or exceed 4 MiB")
		}
		// Windows PowerShell commonly writes a UTF-8 BOM with JSON files.
		decoder := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})))
		decoder.UseNumber()
		var arguments map[string]any
		if err := decoder.Decode(&arguments); err != nil || arguments == nil {
			return errors.New("tool arguments must be a JSON object (use {} for no arguments)")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return errors.New("tool arguments must contain exactly one JSON object")
		}
		call = &toolcli.Call{Name: name, Arguments: arguments}
	default:
		return errors.New("unknown tools command: use guide, list or call")
	}
	config, err := readContext(*contextFile)
	if err != nil {
		return err
	}
	response, err := toolcli.DoQuery(ctx, config, query, call)
	if err != nil {
		return err
	}
	if response.Error != "" {
		if response.Status == http.StatusUnauthorized {
			return errors.New("Workass rejected this context's credential: the chat that launched this process was deleted, or the context was issued by an engine that has since stopped. For delayed work, ask the chat to use workass_schedule_message instead of a detached helper")
		}
		return errors.New(acp.RedactSensitiveText(response.Error))
	}
	result := response.Result
	if len(response.Images) > 0 {
		// Native shell tools return text. Materialize image bytes so the harness
		// can inspect them with its built-in image reader, without an MCP block.
		var images []map[string]string
		for _, image := range response.Images {
			ext := ""
			switch image.MIMEType {
			case "image/png":
				ext = ".png"
			case "image/jpeg":
				ext = ".jpg"
			case "image/webp":
				ext = ".webp"
			default:
				return errors.New("unsupported Workass image type")
			}
			data, err := base64.StdEncoding.DecodeString(image.Data)
			if err != nil {
				return errors.New("invalid Workass image bytes")
			}
			f, err := os.CreateTemp(filepath.Dir(*contextFile), "tool-image-*"+ext)
			if err != nil {
				return errors.New("cannot save Workass tool image")
			}
			_, writeErr := f.Write(data)
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(f.Name())
				return errors.New("cannot save Workass tool image")
			}
			images = append(images, map[string]string{"path": f.Name(), "mime_type": image.MIMEType})
		}
		result = map[string]any{"result": result, "images": images}
	}
	// Agents copy URLs out of tool results, so & < > stay literal instead of
	// &-style escapes, matching the Windows tools client.
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}
