package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"workass/internal/connectors"
	"workass/internal/wire"
)

// Conectores + alerts (docs/specs/connectors-alerts.md). The daemon that
// listens owns its connectors (state/connectors) and their alerts; every
// client reads and decides through these channels. Nothing here touches a
// chat: an alert reaches an agent only after the owner hands it over from the
// client, which then records the link with alerts:act.
//   - connectors:list / alerts:list are reads.
//   - connectors:test fetches Grafana once (10 s bound), so it is a
//     concurrent read that never holds the ordered lane.
//   - connectors:set, connectors:remove, connectors:rotate-secret and
//     alerts:act are mutating (internal/wire mutatingChannels).
//   - events connectors:changed {connectors} and alerts:changed {alerts,
//     announce} come from the service itself.

const connectorsUnavailable = "connectors are unavailable on this machine"

func decodeConnectorArg(args []any, into any) error {
	raw, err := json.Marshal(firstMapArg(args))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

func registerConnectorHandlers(hub *wire.Hub, svc *connectors.Service) int {
	unavailable := errors.New(connectorsUnavailable)
	hub.Register("connectors:list", func(args []any) (any, error) {
		if svc == nil {
			return map[string]any{"connectors": []connectors.Connector{}}, nil
		}
		return map[string]any{"connectors": svc.List()}, nil
	})
	hub.Register("connectors:set", func(args []any) (any, error) {
		if svc == nil {
			return nil, unavailable
		}
		var req connectors.SetRequest
		if err := decodeConnectorArg(args, &req); err != nil {
			return nil, errors.New("connectors:set needs {name, url, mode, ...}")
		}
		connector, secret, err := svc.Set(req)
		if err != nil {
			return nil, err
		}
		reply := map[string]any{"connector": connector}
		if secret != "" {
			reply["webhookSecret"] = secret
		}
		return reply, nil
	})
	hub.Register("connectors:remove", func(args []any) (any, error) {
		if svc == nil {
			return nil, unavailable
		}
		if err := svc.Remove(fieldString(firstMapArg(args), "id")); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
	hub.RegisterConcurrentRead("connectors:test", func(args []any) (any, error) {
		if svc == nil {
			return nil, unavailable
		}
		var req connectors.TestRequest
		if err := decodeConnectorArg(args, &req); err != nil {
			return nil, errors.New("connectors:test needs {id} or {url, token}")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return svc.Test(ctx, req), nil
	})
	hub.Register("connectors:rotate-secret", func(args []any) (any, error) {
		if svc == nil {
			return nil, unavailable
		}
		secret, err := svc.RotateSecret(fieldString(firstMapArg(args), "id"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"webhookSecret": secret}, nil
	})
	hub.Register("alerts:list", func(args []any) (any, error) {
		if svc == nil {
			return map[string]any{"alerts": []connectors.Alert{}}, nil
		}
		return map[string]any{"alerts": svc.Alerts()}, nil
	})
	hub.Register("alerts:act", func(args []any) (any, error) {
		if svc == nil {
			return nil, unavailable
		}
		var req connectors.ActRequest
		if err := decodeConnectorArg(args, &req); err != nil {
			return nil, errors.New("alerts:act needs {ids, action}")
		}
		changed, err := svc.Act(req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"alerts": changed}, nil
	})
	return 7
}

// connectorWebhookHandler is the daemon mux route for Grafana's webhook
// contact point. Without a service every connector is unknown.
func connectorWebhookHandler(svc *connectors.Service) http.Handler {
	if svc == nil {
		return http.HandlerFunc(http.NotFound)
	}
	return svc.WebhookHandler()
}
