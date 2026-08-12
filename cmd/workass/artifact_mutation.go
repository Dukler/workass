package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"workass/internal/acp"
	"workass/internal/artifacthost"
	providercontract "workass/internal/provider"
)

const (
	artifactMutationKind    = "artifact:host"
	artifactMutationMethod  = "artifact.host"
	maxArtifactReceiptBytes = 4096
)

// hostArtifact is the agent-control boundary for artifact hosting. It fences
// the exact actor and owner first, records only a safe immutable digest, then
// lets the generic external-mutation executor claim the journal before the
// registry is allowed to inspect or capture the source.
func (h *agentControlHandler) hostArtifact(r *http.Request, ownerKey, tabID, chatID string, params map[string]any) (any, error) {
	if h == nil || h.artifacts == nil {
		return nil, errors.New("Workass artifact hosting is unavailable")
	}
	if h.chats == nil || h.chats.providerChats == nil {
		return nil, errors.New("artifact hosting requires the durable chat actor")
	}
	if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerReadError); err != nil {
		return nil, err
	}
	operationID, operationErr := providercontract.ValidateOperationID(fieldString(params, "operation_id"))
	if operationErr != nil {
		return nil, errors.New("artifact.host requires a valid caller-stable operation_id")
	}
	sourcePath := strings.TrimSpace(fieldString(params, "source_path"))
	entry := strings.TrimSpace(fieldString(params, "entry"))
	name := strings.TrimSpace(fieldString(params, "name"))
	digest := artifactHostRequestDigest(tabID, chatID, sourcePath, entry, name)
	cwd, err := h.chats.providerChats.AgentOwnerCWD(ownerKey, tabID, chatID)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	reply, err := h.chats.providerChats.executeBrowserMutation(
		ctx, tabID, chatID, operationID, artifactMutationKind, artifactMutationMethod, digest,
		func() (browserControlReply, error) {
			registration, registerErr := h.artifacts.RegisterForOperation(
				artifacthost.RegisterOptions{BaseDir: cwd, SourcePath: sourcePath, Entry: entry, Label: acp.RedactSensitiveText(name)},
				string(operationID), digest,
			)
			if registerErr != nil {
				// Registration validation is a definite rejection. Return a bounded
				// receipt so the actor settles Failed instead of making a later call
				// guess whether filesystem validation escaped.
				return artifactMutationFailureReply(operationID, digest, registerErr), nil
			}
			return artifactRegistrationReply(registration, operationID, digest)
		},
		func() (browserControlReply, error) {
			return readArtifactRegistration(h.artifacts, string(operationID), digest)
		},
	)
	if err != nil {
		return nil, err
	}
	if reply.Error != "" {
		return nil, errors.New(acp.RedactSensitiveText(reply.Error))
	}
	if registration, ok := reply.Result.(artifacthost.Registration); ok {
		return registration, nil
	}
	// A completed actor entry is a bounded receipt, not the registration body.
	// Rebuild the public result from the registry's operation readback; this is
	// also the same path used after a lost reply and daemon restart.
	return readArtifactRegistrationResult(h.artifacts, string(operationID), digest)
}

type artifactHostRequest struct {
	Version    int    `json:"version"`
	TabID      string `json:"tabId"`
	ChatID     string `json:"chatId"`
	SourcePath string `json:"sourcePath"`
	Entry      string `json:"entry"`
	Name       string `json:"name"`
}

func artifactHostRequestDigest(tabID, chatID, sourcePath, entry, name string) string {
	payload := artifactHostRequest{
		Version: 1, TabID: strings.TrimSpace(tabID), ChatID: strings.TrimSpace(chatID),
		SourcePath: strings.TrimSpace(sourcePath), Entry: strings.TrimSpace(entry), Name: strings.TrimSpace(name),
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(append([]byte("workass-artifact-host-v1\x00"), raw...))
	return hex.EncodeToString(sum[:])
}

func artifactMutationFailureReply(operationID providercontract.OperationID, digest string, err error) browserControlReply {
	message := acp.RedactSensitiveText(strings.TrimSpace(err.Error()))
	if len([]byte(message)) > maxArtifactReceiptBytes {
		message = message[:maxArtifactReceiptBytes]
	}
	return browserControlReply{OperationID: string(operationID), RequestDigest: digest, Receipt: true, Error: message}
}

func artifactRegistrationReply(registration artifacthost.Registration, operationID providercontract.OperationID, digest string) (browserControlReply, error) {
	safe, err := boundedArtifactRegistration(registration)
	if err != nil {
		return browserControlReply{}, err
	}
	return browserControlReply{Result: safe, OperationID: string(operationID), RequestDigest: digest, Receipt: true}, nil
}

func boundedArtifactRegistration(registration artifacthost.Registration) (artifacthost.Registration, error) {
	safe := registration
	var err error
	for name, value := range map[string]*string{
		"label": &safe.Label, "entry": &safe.Entry, "content type": &safe.ContentType,
		"url path": &safe.URLPath, "local url": &safe.LocalURL, "markdown": &safe.Markdown,
		"created at": &safe.CreatedAt, "updated at": &safe.UpdatedAt,
	} {
		*value, err = boundedArtifactReceiptField(*value)
		if err != nil {
			return artifacthost.Registration{}, errors.New("artifact registration receipt field " + name + " is invalid")
		}
	}
	if len([]byte(safe.ID)) > maxArtifactReceiptBytes || strings.ContainsAny(safe.ID, "\x00\r\n") ||
		safe.ID == "" || !strings.HasPrefix(safe.URLPath, artifacthost.PathPrefix+"/") || strings.Contains(safe.URLPath, "://") {
		return artifacthost.Registration{}, errors.New("artifact registration receipt is invalid")
	}
	if len(safe.Withheld) > 20 || safe.WithheldMore < 0 {
		return artifacthost.Registration{}, errors.New("artifact registration receipt is too large")
	}
	for index := range safe.Withheld {
		safe.Withheld[index].Path, err = boundedArtifactReceiptField(safe.Withheld[index].Path)
		if err != nil {
			return artifacthost.Registration{}, errors.New("artifact withheld path is invalid")
		}
		safe.Withheld[index].Reason, err = boundedArtifactReceiptField(safe.Withheld[index].Reason)
		if err != nil {
			return artifacthost.Registration{}, errors.New("artifact withheld reason is invalid")
		}
	}
	return safe, nil
}

func boundedArtifactReceiptField(value string) (string, error) {
	value = acp.RedactSensitiveText(strings.TrimSpace(value))
	if len([]byte(value)) > maxArtifactReceiptBytes || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("bounded artifact receipt field is invalid")
	}
	return value, nil
}

func readArtifactRegistration(registry *artifacthost.Registry, operationID, digest string) (browserControlReply, error) {
	registration, found, err := registry.ReadOperation(operationID, digest)
	if err != nil {
		return browserControlReply{}, err
	}
	if !found {
		return browserControlReply{}, errors.New("artifact registration readback is unavailable")
	}
	return artifactRegistrationReply(registration, providercontract.OperationID(operationID), digest)
}

func readArtifactRegistrationResult(registry *artifacthost.Registry, operationID, digest string) (any, error) {
	reply, err := readArtifactRegistration(registry, operationID, digest)
	if err != nil {
		return nil, err
	}
	if reply.Error != "" {
		return nil, errors.New(acp.RedactSensitiveText(reply.Error))
	}
	registration, ok := reply.Result.(artifacthost.Registration)
	if !ok {
		return nil, errors.New("artifact registration readback returned an invalid receipt")
	}
	return registration, nil
}
