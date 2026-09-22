package main

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"syscall"

	"workass/internal/acp"
	"workass/internal/toolcli"
	"workass/internal/toolcommand"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "tools" {
		_ = json.NewEncoder(os.Stderr).Encode(toolcli.Response{Error: "a leading tools command is required"})
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := toolcommand.Run(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(toolcli.Response{Error: acp.RedactSensitiveText(err.Error())})
		os.Exit(1)
	}
}
