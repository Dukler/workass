package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"workass/internal/agentserver"
	"workass/internal/lmstudio"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "workass-agent: %s\n", agentserver.RedactSensitiveText(err.Error()))
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "serve-acp" {
		args = args[1:]
	}
	env := lmstudio.ConfigFromEnv()
	cfg := env
	var probe bool
	fs := flag.NewFlagSet("workass-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.BaseURL, "openai-base-url", env.BaseURL, "OpenAI-compatible API base URL, for example http://127.0.0.1:1234/v1")
	fs.StringVar(&cfg.Model, "openai-model", env.Model, "OpenAI-compatible model id")
	fs.StringVar(&cfg.APIKey, "openai-api-key", env.APIKey, "OpenAI-compatible API key")
	fs.BoolVar(&probe, "probe", false, "print a self-test JSON object to stderr and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := lmstudio.New(cfg)
	if err != nil {
		if probe {
			writeProbe(stderr, cfg, nil, err)
			return nil
		}
		return err
	}
	if probe {
		probeClient(stderr, cfg, client)
		return nil
	}
	server, err := agentserver.New(agentserver.Options{
		Client:           client,
		ClientConfig:     cfg,
		Stdout:           stdout,
		Stderr:           stderr,
		SessionStorePath: strings.TrimSpace(os.Getenv("WORKASS_AGENT_SESSION_STORE")),
	})
	if err != nil {
		return err
	}
	return server.Serve(context.Background(), stdin)
}

func probeClient(stderr io.Writer, cfg lmstudio.Config, client *lmstudio.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	models, err := client.ListModels(ctx)
	writeProbe(stderr, cfg, models, err)
}

func writeProbe(stderr io.Writer, cfg lmstudio.Config, models []lmstudio.Model, err error) {
	result := map[string]any{
		"ok":               err == nil,
		"baseURL":          cfg.BaseURL,
		"model":            cfg.Model,
		"apiKeyConfigured": cfg.APIKey != "",
		"modelCount":       len(models),
	}
	if err != nil {
		result["error"] = agentserver.RedactSensitiveText(err.Error())
	}
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		_, _ = fmt.Fprintf(stderr, `{"ok":false,"error":"%s"}`+"\n", agentserver.RedactSensitiveText(marshalErr.Error()))
		return
	}
	_, _ = stderr.Write(append(data, '\n'))
}
