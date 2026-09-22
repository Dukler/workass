package main

import (
	"context"
	"io"
	"strings"

	"workass/internal/toolcommand"
)

func runToolsCommand(ctx context.Context, args []string, input io.Reader, output, diagnostics io.Writer) error {
	return toolcommand.Run(ctx, args, input, output, diagnostics)
}

func bearerCredential(value string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	credential := strings.TrimPrefix(value, prefix)
	return credential, credential != "" && strings.TrimSpace(credential) == credential
}
