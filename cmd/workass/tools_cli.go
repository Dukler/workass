package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"workass/internal/acp"
	"workass/internal/toolcli"
)

func runToolsCommand(ctx context.Context, args []string, input io.Reader, output, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("workass tools", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	contextFile := flags.String("context", "", "private context file supplied by the current Workass turn")
	flags.Usage = func() {
		fmt.Fprintln(diagnostics, "workass tools --context FILE list [NAME]\nworkass tools --context FILE call NAME [--input FILE]\nCall arguments are one JSON object read from stdin, or --input FILE. Mutations require operation_id; retry only with the same id and arguments.")
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
	switch args[0] {
	case "list":
		if len(args) > 2 {
			return errors.New("list accepts at most one exact tool name")
		}
		if len(args) == 2 {
			name = args[1]
		}
	case "call":
		if len(args) < 2 {
			return errors.New("call requires an exact tool name")
		}
		name = args[1]
		callFlags := flag.NewFlagSet("call", flag.ContinueOnError)
		callFlags.SetOutput(diagnostics)
		inputFile := callFlags.String("input", "", "JSON arguments file; stdin by default")
		if err := callFlags.Parse(args[2:]); err != nil {
			return err
		}
		if callFlags.NArg() != 0 {
			return errors.New("unexpected call argument")
		}
		if *inputFile != "" {
			f, err := os.Open(*inputFile)
			if err != nil {
				return errors.New("cannot open tool arguments file")
			}
			defer f.Close()
			input = f
		}
		raw, err := io.ReadAll(io.LimitReader(input, 4*1024*1024+1))
		if err != nil || len(raw) > 4*1024*1024 {
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
		return errors.New("unknown tools command: use list or call")
	}
	config, err := toolcli.ReadConfig(*contextFile)
	if err != nil {
		return err
	}
	response, err := toolcli.Do(ctx, config, name, call)
	if err != nil {
		return err
	}
	if response.Error != "" {
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
	return json.NewEncoder(output).Encode(result)
}

func bearerCredential(value string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	credential := strings.TrimPrefix(value, prefix)
	return credential, credential != "" && strings.TrimSpace(credential) == credential
}
