package main

import (
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"

	"workass/internal/artifacthost"
	"workass/internal/wire"
)

const (
	maxVisualizeFragmentBytes = 1 * 1024 * 1024
	maxVisualizeTitleBytes    = 200
)

const visualizeCSP = "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'; script-src 'unsafe-inline' https://cdnjs.cloudflare.com https://esm.sh https://cdn.jsdelivr.net https://unpkg.com; style-src 'unsafe-inline' https://cdnjs.cloudflare.com https://esm.sh https://cdn.jsdelivr.net https://unpkg.com https://fonts.googleapis.com https://fonts.gstatic.com https://fonts.bunny.net; font-src https://fonts.googleapis.com https://fonts.gstatic.com https://fonts.bunny.net; img-src data: blob: https://cdnjs.cloudflare.com https://esm.sh https://cdn.jsdelivr.net https://unpkg.com https://fonts.googleapis.com https://fonts.gstatic.com https://fonts.bunny.net; media-src data: blob:; connect-src 'none'; frame-src 'none'; navigate-to 'none'"

func registerVisualizeHandler(hub *wire.Hub, registry *artifacthost.Registry, sessionState *sessionStore, stateDir string) {
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
		if sessionState == nil {
			return nil, fmt.Errorf("visualization chat ownership is unavailable")
		}
		if _, ok := sessionState.ChatWorkspace(tabID, chatID); !ok {
			return nil, fmt.Errorf("visualization chat is not known to this daemon")
		}

		sourcePath, err := resolveVisualizationPath(fieldString(arg, "path"), stateDir)
		if err != nil {
			return nil, err
		}
		fragment, err := readVisualizationFragment(sourcePath)
		if err != nil {
			return nil, err
		}
		mode := strings.TrimSpace(fieldString(arg, "mode"))
		if mode != "" && mode != "wide" {
			return nil, fmt.Errorf("visualization mode must be wide when provided")
		}
		title := strings.TrimSpace(fieldString(arg, "title"))
		if title == "" {
			title = "Visualization"
		}
		if len([]byte(title)) > maxVisualizeTitleBytes || strings.ContainsAny(title, "\x00\r\n") {
			return nil, fmt.Errorf("visualization title is too long or contains control characters")
		}
		wrapped := wrapVisualizationHTML(title, fragment)
		registration, err := registry.RegisterCapturedHTML(title, wrapped)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"id":          registration.ID,
			"label":       registration.Label,
			"entry":       registration.Entry,
			"contentType": registration.ContentType,
			"urlPath":     registration.URLPath,
			"localUrl":    registration.LocalURL,
			"markdown":    registration.Markdown,
			"createdAt":   registration.CreatedAt,
			"updatedAt":   registration.UpdatedAt,
			"mode":        mode,
			"title":       title,
		}, nil
	})
}

func resolveVisualizationPath(rawPath, stateDir string) (string, error) {
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
	for _, root := range visualizationRoots(stateDir) {
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
	return roots
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
