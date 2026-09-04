package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"

	"workass/internal/acp"
	"workass/internal/artifacthost"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

const (
	maxVisualizeFragmentBytes = 1 * 1024 * 1024
	maxVisualizeTitleBytes    = 200
	maxVisualizeReceiptBytes  = 4096
	visualizeMutationKind     = "visualize:host"
	visualizeMutationPrefix   = "visualize-html-v1:"
	visualizeOperationPrefix  = "visualize-host-v1:"
)

const visualizeCSP = "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'; script-src 'unsafe-inline' https://cdnjs.cloudflare.com https://esm.sh https://cdn.jsdelivr.net https://unpkg.com; style-src 'unsafe-inline' https://cdnjs.cloudflare.com https://esm.sh https://cdn.jsdelivr.net https://unpkg.com https://fonts.googleapis.com https://fonts.gstatic.com https://fonts.bunny.net; font-src https://fonts.googleapis.com https://fonts.gstatic.com https://fonts.bunny.net; img-src data: blob: https://cdnjs.cloudflare.com https://esm.sh https://cdn.jsdelivr.net https://unpkg.com https://fonts.googleapis.com https://fonts.gstatic.com https://fonts.bunny.net; media-src data: blob:; connect-src 'none'; frame-src 'none'; navigate-to 'none'"

func registerVisualizeHandler(hub *wire.Hub, registry *artifacthost.Registry, providerChats *providerChatRuntime, stateDir string) {
	hub.Register("visualize:host", func(args []any) (any, error) {
		if registry == nil {
			return nil, fmt.Errorf("visualization hosting is unavailable")
		}
		arg := firstMapArg(args)
		tabID := fieldString(arg, "tabId")
		chatID := fieldString(arg, "chatId")
		if tabID == "" || chatID == "" {
			return nil, fmt.Errorf("visualize:host requires tabId and chatId")
		}
		if providerChats == nil {
			return nil, fmt.Errorf("visualization chat ownership is unavailable")
		}
		_, state, err := providerChats.exactActor(tabID, chatID)
		if err != nil {
			return nil, fmt.Errorf("visualization chat is not known to this daemon: %w", err)
		}
		if !state.Initialized || state.Deleted {
			return nil, errors.New("visualization chat is not an active actor")
		}
		workspaceCWD := visualizationWorkspaceCWD(state)

		mode, title, err := visualizeRequestPresentation(arg)
		if err != nil {
			return nil, err
		}
		rawPath := fieldString(arg, "path")
		identityDigest := visualizeRequestIdentityDigest(tabID, chatID, rawPath, mode, title)
		callerOperationID := strings.TrimSpace(fieldString(arg, "operationId"))
		var operationID providercontract.OperationID
		var existing chat.OutboxEntry
		var exists bool
		var capture visualizeCapture
		captureReady := false
		var prepareErr error

		if callerOperationID != "" {
			operationID, err = visualizeOperationID(arg, identityDigest)
			if err != nil {
				return nil, err
			}
			existing, exists = externalBrowserMutationEntry(state, operationID)
			// A journaled terminal operation is authoritative. In particular, a
			// completed retry must not reread a source that may have disappeared or
			// changed after the captured effect was committed. A new or pending
			// operation still prepares the capture so its immutable target can be
			// checked before dispatch.
			if !exists || existing.Status == chat.OutboxPending {
				capture, prepareErr = prepareVisualizeCaptureForWorkspace(rawPath, stateDir, workspaceCWD, mode, title, identityDigest)
				captureReady = prepareErr == nil
			}
		} else {
			// The frozen bridge does not carry an operation id. A readable source
			// lets the immutable captured-content digest participate in the
			// derived id, so overwriting one temp path is a new request rather than
			// a permanent reuse conflict.
			capture, prepareErr = prepareVisualizeCaptureForWorkspace(rawPath, stateDir, workspaceCWD, mode, title, identityDigest)
			captureReady = prepareErr == nil
			if captureReady {
				operationID = derivedVisualizeOperationID(identityDigest, capture)
				existing, exists = externalBrowserMutationEntry(state, operationID)
			} else {
				// On an exact lost-reply retry the executor-owned source may have
				// disappeared. Recover the one prior operation by its safe immutable
				// request identity; never guess among multiple content versions.
				existing, exists, err = visualizeFallbackJournal(state, identityDigest)
				if err != nil {
					return nil, err
				}
				if !exists {
					return nil, safeVisualizeError(prepareErr)
				}
				operationID = existing.OperationID
			}
		}

		if exists {
			storedIdentity, _, ok := visualizeMutationParts(existing.MutationMethod)
			if !ok || storedIdentity != identityDigest {
				return nil, errors.New("visualize:host operation id was reused for different content")
			}
			if captureReady && (existing.MutationMethod != capture.method || existing.MutationDigest != capture.digest) {
				return nil, errors.New("visualize:host operation id was reused for different content")
			}
			if existing.Status != chat.OutboxPending {
				if existing.Status == chat.OutboxCompleted {
					// executeBrowserMutation intentionally returns only its bounded
					// generic actor receipt for Completed. The artifact registry owns the
					// safe, durable registration readback needed to rebuild its URL/result.
					reply, err := readVisualizeRegistration(registry, mode, title, operationID, existing.MutationDigest)
					if err != nil {
						return nil, err
					}
					return visualizeReplyResult(reply)
				}
				if existing.Status == chat.OutboxFailed {
					reply, err := providerChats.executeBrowserMutation(
						context.Background(), tabID, chatID, operationID, visualizeMutationKind,
						existing.MutationMethod, existing.MutationDigest, nil, nil,
					)
					if err != nil {
						return nil, err
					}
					return visualizeReplyResult(reply)
				}
				reply, err := providerChats.executeBrowserMutation(
					context.Background(), tabID, chatID, operationID, visualizeMutationKind,
					existing.MutationMethod, existing.MutationDigest,
					nil,
					func() (browserControlReply, error) {
						return readVisualizeRegistration(registry, mode, title, operationID, existing.MutationDigest)
					},
				)
				if err != nil {
					return nil, err
				}
				return visualizeReplyResult(reply)
			}
			if !captureReady {
				// A pending operation has not escaped to the registry, but its
				// source vanished before dispatch. Claim it and fail closed; never
				// manufacture a second capture from a changed source.
				_, err := providerChats.executeBrowserMutation(
					context.Background(), tabID, chatID, operationID, visualizeMutationKind,
					existing.MutationMethod, existing.MutationDigest,
					nil,
					func() (browserControlReply, error) {
						return readVisualizeRegistration(registry, mode, title, operationID, existing.MutationDigest)
					},
				)
				return nil, err
			}
		}
		if !captureReady {
			return nil, safeVisualizeError(prepareErr)
		}

		reply, err := providerChats.executeBrowserMutation(
			context.Background(), tabID, chatID, operationID, visualizeMutationKind,
			capture.method, capture.digest,
			func() (browserControlReply, error) {
				registration, err := registry.RegisterCapturedHTMLForOperation(title, capture.wrapped, string(operationID), capture.digest)
				if err != nil {
					return browserControlReply{}, err
				}
				return visualizeRegistrationReply(registration, mode, title, operationID, capture.digest)
			},
			func() (browserControlReply, error) {
				return readVisualizeRegistration(registry, mode, title, operationID, capture.digest)
			},
		)
		if err != nil {
			return nil, err
		}
		if result, ok := reply.Result.(map[string]any); ok {
			return result, nil
		}
		return visualizeReplyResult(reply)
	})
}

type visualizeCapture struct {
	wrapped       []byte
	captureKey    string
	contentDigest string
	method        string
	digest        string
}

func visualizeRequestPresentation(arg map[string]any) (string, string, error) {
	mode := strings.TrimSpace(fieldString(arg, "mode"))
	if mode != "" && mode != "wide" {
		return "", "", fmt.Errorf("visualization mode must be wide when provided")
	}
	title := acp.RedactSensitiveText(strings.TrimSpace(fieldString(arg, "title")))
	if title == "" {
		title = "Visualization"
	}
	if len([]byte(title)) > maxVisualizeTitleBytes || strings.ContainsAny(title, "\x00\r\n") {
		return "", "", fmt.Errorf("visualization title is too long or contains control characters")
	}
	return mode, title, nil
}

// Normalize a caller-supplied operation id. The frozen bridge fallback is
// derivedVisualizeOperationID, which is called only after the captured content
// digest is available.
func visualizeOperationID(arg map[string]any, identityDigest string) (providercontract.OperationID, error) {
	raw := strings.TrimSpace(fieldString(arg, "operationId"))
	if raw == "" {
		return "", errors.New("visualize:host operationId is unavailable")
	}
	if len([]byte(raw)) > 128 || strings.ContainsAny(raw, "\x00\r\n") {
		return "", errors.New("visualize:host operationId is invalid")
	}
	// Operation ids are actor state. Do not persist a caller value that is
	// itself a path, URL, or secret-shaped string; retain stable equality with
	// an opaque digest instead.
	if filepath.IsAbs(raw) || strings.ContainsAny(raw, `/\\<>`) || strings.Contains(raw, "://") || acp.MayContainSecret(raw) {
		sum := sha256.Sum256(append([]byte("workass-visualize-operation-v1\x00"), []byte(raw)...))
		return providercontract.OperationID("visualize-host-op-" + hex.EncodeToString(sum[:])), nil
	}
	return providercontract.NormalizeOperationID(raw), nil
}

func visualizeRequestIdentityDigest(tabID, chatID, rawPath, mode, title string) string {
	payload := struct {
		Version int
		TabID   string
		ChatID  string
		Path    string
		Mode    string
		Title   string
	}{Version: 1, TabID: tabID, ChatID: chatID, Path: strings.TrimSpace(rawPath), Mode: mode, Title: title}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(append([]byte("workass-visualize-identity-v1\x00"), raw...))
	return hex.EncodeToString(sum[:])
}

func prepareVisualizeCapture(rawPath, stateDir, mode, title, identityDigest string) (visualizeCapture, error) {
	return prepareVisualizeCaptureForWorkspace(rawPath, stateDir, "", mode, title, identityDigest)
}

func prepareVisualizeCaptureForWorkspace(rawPath, stateDir, workspaceCWD, mode, title, identityDigest string) (visualizeCapture, error) {
	sourcePath, err := resolveVisualizationPathForWorkspace(rawPath, stateDir, workspaceCWD)
	if err != nil {
		return visualizeCapture{}, err
	}
	fragment, err := readVisualizationFragment(sourcePath)
	if err != nil {
		return visualizeCapture{}, err
	}
	wrapped := wrapVisualizationHTML(title, fragment)
	contentSum := sha256.Sum256(wrapped)
	payload := struct {
		Version       int
		Identity      string
		ResolvedPath  string
		ContentDigest string
		ContentBytes  int
		Mode          string
	}{Version: 1, Identity: identityDigest, ResolvedPath: sourcePath, ContentDigest: hex.EncodeToString(contentSum[:]), ContentBytes: len(wrapped), Mode: mode}
	raw, _ := json.Marshal(payload)
	digestSum := sha256.Sum256(append([]byte("workass-visualize-request-v1\x00"), raw...))
	digest := hex.EncodeToString(digestSum[:])
	captureSum := sha256.Sum256(append([]byte(strings.TrimSpace(title)+"\x00"), wrapped...))
	captureKey := hex.EncodeToString(captureSum[:12])
	contentDigest := hex.EncodeToString(contentSum[:])
	return visualizeCapture{
		wrapped: wrapped, captureKey: captureKey,
		contentDigest: contentDigest,
		method:        visualizeMutationPrefix + identityDigest + ":" + captureKey,
		digest:        digest,
	}, nil
}

func derivedVisualizeOperationID(identityDigest string, capture visualizeCapture) providercontract.OperationID {
	return providercontract.OperationID(visualizeOperationPrefix + identityDigest + ":" + capture.contentDigest)
}

func visualizeMutationParts(method string) (string, string, bool) {
	if !strings.HasPrefix(method, visualizeMutationPrefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(method, visualizeMutationPrefix), ":")
	if len(parts) != 2 || len(parts[0]) != sha256.Size*2 || len(parts[1]) != 24 {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", "", false
	}
	return strings.ToLower(parts[0]), strings.ToLower(parts[1]), true
}

func visualizeFallbackJournal(state chat.State, identityDigest string) (chat.OutboxEntry, bool, error) {
	var match chat.OutboxEntry
	count := 0
	for _, entry := range state.Outbox {
		if entry.Kind != chat.EffectExternalMutation || entry.MutationKind != visualizeMutationKind {
			continue
		}
		storedIdentity, _, ok := visualizeMutationParts(entry.MutationMethod)
		if !ok || storedIdentity != strings.ToLower(strings.TrimSpace(identityDigest)) {
			continue
		}
		match = entry
		count++
	}
	switch count {
	case 0:
		return chat.OutboxEntry{}, false, nil
	case 1:
		return match, true, nil
	default:
		return chat.OutboxEntry{}, false, errors.New("visualize:host retry is ambiguous")
	}
}

func visualizeRegistrationReply(registration artifacthost.Registration, mode, title string, operationID providercontract.OperationID, digest string) (browserControlReply, error) {
	result, err := visualizeRegistrationPayload(registration, mode, title)
	if err != nil {
		return browserControlReply{}, err
	}
	return browserControlReply{Result: result, OperationID: string(operationID), RequestDigest: digest, Receipt: true}, nil
}

func visualizeRegistrationPayload(registration artifacthost.Registration, mode, title string) (map[string]any, error) {
	fields := map[string]string{
		"id": registration.ID, "label": registration.Label, "entry": registration.Entry,
		"contentType": registration.ContentType, "urlPath": registration.URLPath,
		"localUrl": registration.LocalURL, "markdown": registration.Markdown,
		"createdAt": registration.CreatedAt, "updatedAt": registration.UpdatedAt,
	}
	for key, value := range fields {
		value = acp.RedactSensitiveText(strings.TrimSpace(value))
		if len([]byte(value)) > maxVisualizeReceiptBytes || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("visualization registration receipt field %q is invalid", key)
		}
		fields[key] = value
	}
	if fields["id"] == "" || fields["urlPath"] == "" || !strings.HasPrefix(fields["urlPath"], artifacthost.PathPrefix+"/") || strings.Contains(fields["urlPath"], "://") {
		return nil, errors.New("visualization registration receipt is invalid")
	}
	return map[string]any{
		"id": fields["id"], "label": fields["label"], "entry": fields["entry"],
		"contentType": fields["contentType"], "urlPath": fields["urlPath"],
		"localUrl": fields["localUrl"], "markdown": fields["markdown"],
		"createdAt": fields["createdAt"], "updatedAt": fields["updatedAt"],
		"mode": mode, "title": title,
	}, nil
}

func readVisualizeRegistration(registry *artifacthost.Registry, mode, title string, operationID providercontract.OperationID, digest string) (browserControlReply, error) {
	if registry == nil {
		return browserControlReply{}, errors.New("visualization registration receipt is unavailable")
	}
	registration, found, err := registry.ReadOperation(string(operationID), digest)
	if err != nil {
		return browserControlReply{}, err
	}
	if !found {
		return browserControlReply{}, errors.New("visualization registration receipt is unavailable")
	}
	return visualizeRegistrationReply(registration, mode, title, operationID, digest)
}

func visualizeReplyResult(reply browserControlReply) (map[string]any, error) {
	if strings.TrimSpace(reply.Error) != "" {
		return nil, errors.New(acp.RedactSensitiveText(strings.TrimSpace(reply.Error)))
	}
	result, ok := reply.Result.(map[string]any)
	if !ok {
		return nil, errors.New("visualization registration receipt is invalid")
	}
	return result, nil
}

func safeVisualizeError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(acp.RedactSensitiveText(err.Error()))
}

func resolveVisualizationPath(rawPath, stateDir string) (string, error) {
	return resolveVisualizationPathForWorkspace(rawPath, stateDir, "")
}

func resolveVisualizationPathForWorkspace(rawPath, stateDir, workspaceCWD string) (string, error) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return "", fmt.Errorf("visualization path is required")
	}
	if len(rawPath) > 4096 || strings.ContainsAny(rawPath, "\x00\r\n") || strings.Contains(rawPath, "://") {
		return "", fmt.Errorf("visualization path is invalid")
	}
	if !filepath.IsAbs(rawPath) {
		return "", fmt.Errorf("visualization path must be absolute")
	}
	ext := strings.ToLower(filepath.Ext(rawPath))
	if ext != ".html" && ext != ".htm" {
		return "", fmt.Errorf("visualization path must point to an HTML file")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(rawPath))
	if err != nil {
		return "", fmt.Errorf("visualization path is not readable: %w", err)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("visualization path is not a regular file")
	}
	for _, root := range visualizationRootsForWorkspace(stateDir, workspaceCWD) {
		canonicalRoot, rootErr := filepath.EvalSymlinks(root)
		if rootErr != nil {
			continue
		}
		if visualizationWithin(filepath.Clean(canonicalRoot), resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("visualization path must stay inside Workass visualizations storage")
}

func visualizationRoots(stateDir string) []string {
	return visualizationRootsForWorkspace(stateDir, "")
}

func visualizationRootsForWorkspace(stateDir, workspaceCWD string) []string {
	stateDir = filepath.Clean(strings.TrimSpace(stateDir))
	if stateDir == "." || stateDir == "" {
		return nil
	}
	roots := make([]string, 0, 5)
	current := stateDir
	for i := 0; i < 5 && current != "." && current != string(filepath.Separator); i++ {
		roots = append(roots, filepath.Join(current, "visualizations"))
		next := filepath.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, "Library", "Application Support", "Workass", "visualizations"))
	}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		roots = append(roots, filepath.Join(appData, "Workass", "visualizations"))
	}
	// Visualizations are executor-owned artifacts, not source files. The
	// visualize contract places them in one exact sibling of the chat's working
	// directory so they stay outside the checkout without granting access to an
	// arbitrary parent directory.
	workspaceCWD = filepath.Clean(strings.TrimSpace(workspaceCWD))
	if filepath.IsAbs(workspaceCWD) && workspaceCWD != string(filepath.Separator) {
		roots = append(roots, workspaceCWD+"-visualizations")
	}
	return roots
}

func visualizationWorkspaceCWD(state chat.State) string {
	if state.Presentation.CWD != nil {
		if cwd := strings.TrimSpace(*state.Presentation.CWD); cwd != "" {
			return cwd
		}
	}
	return strings.TrimSpace(state.Environment.CWD)
}

func visualizationWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == "." || rel == ".." {
		return rel == "."
	}
	return rel != "" && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func readVisualizationFragment(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read visualization: %w", err)
	}
	defer file.Close()
	limited := io.LimitReader(file, maxVisualizeFragmentBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read visualization: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("visualization HTML is empty")
	}
	if len(data) > maxVisualizeFragmentBytes {
		return nil, fmt.Errorf("visualization HTML exceeds the %d-byte limit", maxVisualizeFragmentBytes)
	}
	return data, nil
}

func wrapVisualizationHTML(title string, fragment []byte) []byte {
	escapedTitle := html.EscapeString(title)
	prefix := []byte("<!doctype html><html><head><meta charset=\"utf-8\"><meta http-equiv=\"Content-Security-Policy\" content=\"" + visualizeCSP + "\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>" + escapedTitle + "</title><style>html,body{margin:0;min-height:100%;background:transparent}body{box-sizing:border-box;padding:16px}</style></head><body><div id=\"workass-visualization-root\">")
	wrapped := make([]byte, 0, len(prefix)+len(fragment)+len("</div></body></html>"))
	wrapped = append(wrapped, prefix...)
	wrapped = append(wrapped, fragment...)
	wrapped = append(wrapped, []byte("</div></body></html>")...)
	return wrapped
}
