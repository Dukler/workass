package main

import (
	"context"
	"errors"
	"fmt"

	"workass/internal/artifacthost"
	"workass/internal/wire"
)

func registerArtifactTransferHandlers(hub *wire.Hub, registry *artifacthost.Registry) int {
	if hub == nil || registry == nil {
		return 0
	}
	m := artifacthost.NewArtifactTransferManager(registry)
	hub.RegisterOutOfBandRead("artifact:open", func(args []any) (any, error) {
		arg := firstMapArg(args)
		if arg == nil {
			return nil, errors.New("artifact:open requires an object")
		}
		headers := map[string]string{}
		if raw, ok := arg["headers"].(map[string]any); ok {
			for k, v := range raw {
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("artifact header %s must be a string", k)
				}
				headers[k] = s
			}
		}
		return m.Open(context.Background(), fieldString(arg, "path"), fieldString(arg, "method"), headers)
	})
	hub.RegisterOutOfBandRead("artifact:read", func(args []any) (any, error) {
		arg := firstMapArg(args)
		if arg == nil {
			return nil, errors.New("artifact:read requires an object")
		}
		return m.Read(context.Background(), fieldString(arg, "transferId"))
	})
	hub.RegisterOutOfBandRead("artifact:close", func(args []any) (any, error) {
		arg := firstMapArg(args)
		if arg == nil {
			return nil, errors.New("artifact:close requires an object")
		}
		return map[string]any{"closed": true}, m.Close(fieldString(arg, "transferId"))
	})
	return 3
}
