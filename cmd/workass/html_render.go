package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"workass/internal/acp"
	"workass/internal/artifacthost"
	providercontract "workass/internal/provider"
)

// workass_render_html is Workass's route for t3code's html_render: the agent
// publishes a finished page and the chat renders it inline, in the same
// sandboxed frame as a ```html fence (approved mock
// desktop/docs/mocks/html-render). Like t3code, the page is stored once and
// served from a URL; here that is the artifact registry's captured-HTML path
// that `visualize` already uses, under the same operation journal. The wire is
// unchanged: the reference rides in the tool's ordinary JSON result, which
// the provider's shell row already carries, so an older renderer shows only
// that row.
const (
	maxHTMLRenderSourceBytes     = 512_000
	maxHTMLRenderTitleBytes      = 200
	maxHTMLRenderReferenceTitle  = 160
	htmlRenderMutationKind       = "html:render"
	htmlRenderMutationMethod     = "html.render"
	htmlRenderStoreMarker        = "<!--workass-html-render-v1-->"
	htmlRenderResultMessage      = "Shown to the user where this call sits in your reply. Don't announce, describe or restate the page; add only what it doesn't say."
	htmlRenderViewportHeightNote = "The frame grows to fit the page, so a size that follows the viewport makes it grow again and again. Let content set the height, or use fixed pixel heights."
)

// The frame's own policy (desktop/renderer2/src/markdown/html-render.ts,
// HTML_RENDER_CSP). The stored copy carries it too, so the hosted URL has no
// network even when it is opened outside the chat.
const htmlRenderCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; frame-src 'none'; worker-src 'none'; manifest-src 'none'; form-action 'none'; base-uri 'none'; object-src 'none'"

// t3code rejects viewport-height units at publish (apps/server/src/htmlRender/
// HtmlRender.ts:71-97). Only stylesheets and style attributes are checked, so
// page text that mentions a unit passes; comments match first so their
// contents are skipped. An unclosed comment, tag or quote runs to the end.
var (
	htmlRenderCSSSource    = regexp.MustCompile(`(?is)<!--.*?(?:-->|$)|<style\b[^>]*(?:>|$)(.*?)(?:</style\s*>|$)|\sstyle\s*=\s*(?:"([^"]*)(?:"|$)|'([^']*)(?:'|$)|([^\s>]+))`)
	htmlRenderViewportUnit = regexp.MustCompile(`(?i)(?:\d+(?:\.\d+)?|\.\d+)[sld]?vh`)
)

func htmlRenderWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// viewportHeightUnit returns the first viewport-height length in the page's
// CSS, or "". Go's regexp has no lookaround, so t3code's boundaries are
// checked by hand: no word character, '.', '+' or '/' before it (that skips
// base64 runs such as an inlined image containing "1vh"), and no word
// character or '-' after it.
func viewportHeightUnit(page string) string {
	for _, match := range htmlRenderCSSSource.FindAllStringSubmatch(page, -1) {
		css := ""
		for _, group := range match[1:] {
			if group != "" {
				css = group
				break
			}
		}
		for _, at := range htmlRenderViewportUnit.FindAllStringIndex(css, -1) {
			if at[0] > 0 {
				before := css[at[0]-1]
				if htmlRenderWordByte(before) || before == '.' || before == '+' || before == '/' {
					continue
				}
			}
			if at[1] < len(css) {
				after := css[at[1]]
				if htmlRenderWordByte(after) || after == '-' {
					continue
				}
			}
			return css[at[0]:at[1]]
		}
	}
	return ""
}

func htmlRenderTitle(raw string) (string, error) {
	title := acp.RedactSensitiveText(strings.TrimSpace(raw))
	if title == "" {
		return "", errors.New("workass_render_html requires a title")
	}
	if len([]byte(title)) > maxHTMLRenderTitleBytes || strings.ContainsAny(title, "\x00\r\n") {
		return "", fmt.Errorf("workass_render_html title must be one line of at most %d bytes", maxHTMLRenderTitleBytes)
	}
	return title, nil
}

// The title in the result is shortened so the whole reference, JSON-escaped,
// stays inside the bounded head of a tool's output that the chat keeps.
func htmlRenderReferenceTitle(title string) string {
	var out strings.Builder
	size := 0
	for _, char := range title {
		cost := utf8.RuneLen(char)
		switch {
		case char < 0x20:
			continue
		case char == '<' || char == '>' || char == '&':
			cost = 6
		case char == '"' || char == '\\':
			cost = 2
		}
		if size+cost > maxHTMLRenderReferenceTitle {
			break
		}
		size += cost
		out.WriteRune(char)
	}
	return out.String()
}

// The page comes inline (`html`) or from a file the agent wrote (`path`),
// exactly one of them, at most maxHTMLRenderSourceBytes of UTF-8.
func htmlRenderSource(params map[string]any) (string, error) {
	inline, hasInline := params["html"].(string)
	rawPath := strings.TrimSpace(fieldString(params, "path"))
	switch {
	case hasInline && rawPath != "":
		return "", errors.New("workass_render_html takes html or path, not both")
	case !hasInline && rawPath == "":
		return "", errors.New("workass_render_html needs the page as html or path")
	}
	var data []byte
	if rawPath != "" {
		if len(rawPath) > 4096 || strings.ContainsAny(rawPath, "\x00\r\n") || strings.Contains(rawPath, "://") || !filepath.IsAbs(rawPath) {
			return "", errors.New("workass_render_html path must be an absolute file path")
		}
		if ext := strings.ToLower(filepath.Ext(rawPath)); ext != ".html" && ext != ".htm" {
			return "", errors.New("workass_render_html path must name an .html or .htm file")
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(rawPath))
		if err != nil {
			return "", errors.New("workass_render_html path is not readable")
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return "", errors.New("workass_render_html path is not a regular file")
		}
		file, err := os.Open(resolved)
		if err != nil {
			return "", errors.New("workass_render_html path is not readable")
		}
		defer file.Close()
		data, err = io.ReadAll(io.LimitReader(file, maxHTMLRenderSourceBytes+1))
		if err != nil {
			return "", errors.New("workass_render_html path is not readable")
		}
	} else {
		data = []byte(inline)
	}
	if len(data) == 0 || strings.TrimSpace(string(data)) == "" {
		return "", errors.New("workass_render_html page is empty")
	}
	if len(data) > maxHTMLRenderSourceBytes {
		return "", fmt.Errorf("workass_render_html page exceeds the %d-byte limit", maxHTMLRenderSourceBytes)
	}
	if !utf8.Valid(data) {
		return "", errors.New("workass_render_html page must be UTF-8 text")
	}
	return string(data), nil
}

func wrapHTMLRenderPage(source string) []byte {
	return []byte("<!doctype html><meta http-equiv=\"Content-Security-Policy\" content=\"" + htmlRenderCSP + "\">" + htmlRenderStoreMarker + source)
}

func htmlRenderRequestDigest(tabID, chatID, title, contentDigest string) string {
	payload := struct {
		Version       int    `json:"version"`
		TabID         string `json:"tabId"`
		ChatID        string `json:"chatId"`
		Title         string `json:"title"`
		ContentDigest string `json:"contentDigest"`
	}{Version: 1, TabID: strings.TrimSpace(tabID), ChatID: strings.TrimSpace(chatID), Title: title, ContentDigest: contentDigest}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(append([]byte("workass-html-render-v1\x00"), raw...))
	return hex.EncodeToString(sum[:])
}

// htmlRenderResult is the tool's JSON result. Keys sort as written, so the
// reference comes first in the encoded line and survives any cut-off of the
// message after it.
func htmlRenderResult(registration artifacthost.Registration, title string) (map[string]any, error) {
	if registration.ID == "" || !strings.HasPrefix(registration.URLPath, artifacthost.PathPrefix+"/") ||
		!strings.HasSuffix(registration.URLPath, "/"+registration.ID+"/") || strings.Contains(registration.URLPath, "://") {
		return nil, errors.New("workass_render_html registration receipt is invalid")
	}
	return map[string]any{
		"htmlRender": map[string]any{
			"id":      registration.ID,
			"title":   htmlRenderReferenceTitle(title),
			"urlPath": registration.URLPath,
		},
		"message": htmlRenderResultMessage,
	}, nil
}

// renderHTML is the agent-control boundary for workass_render_html. It fences
// the exact actor and owner, validates the page before anything is journaled,
// then stores it through the same external-mutation executor and registry
// receipt as artifact hosting, so a retry with the same operation_id returns
// the same page and never stores a second one.
func (h *agentControlHandler) renderHTML(r *http.Request, ownerKey, tabID, chatID string, params map[string]any) (any, error) {
	if h == nil || h.artifacts == nil {
		return nil, errors.New("Workass page rendering is unavailable")
	}
	if h.chats == nil || h.chats.providerChats == nil {
		return nil, errors.New("page rendering requires the durable chat actor")
	}
	if err := h.authorizeActorOwner(ownerKey, tabID, chatID, agentOwnerReadError); err != nil {
		return nil, err
	}
	operationID, operationErr := providercontract.ValidateOperationID(fieldString(params, "operation_id"))
	if operationErr != nil {
		return nil, errors.New("workass_render_html requires a valid caller-stable operation_id")
	}
	title, err := htmlRenderTitle(fieldString(params, "title"))
	if err != nil {
		return nil, err
	}
	source, err := htmlRenderSource(params)
	if err != nil {
		return nil, err
	}
	if unit := viewportHeightUnit(source); unit != "" {
		return nil, fmt.Errorf("workass_render_html page uses a viewport height unit (%s). %s", unit, htmlRenderViewportHeightNote)
	}
	// The page is shown and stored as the chat shows any agent text: redacted.
	source = acp.RedactSensitiveText(source)
	sum := sha256.Sum256([]byte(source))
	digest := htmlRenderRequestDigest(tabID, chatID, title, hex.EncodeToString(sum[:]))
	wrapped := wrapHTMLRenderPage(source)

	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	reply, err := h.chats.providerChats.executeBrowserMutation(
		ctx, tabID, chatID, operationID, htmlRenderMutationKind, htmlRenderMutationMethod, digest,
		func() (browserControlReply, error) {
			registration, registerErr := h.artifacts.RegisterCapturedHTMLForOperation(title, wrapped, string(operationID), digest)
			if registerErr != nil {
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
	registration, ok := reply.Result.(artifacthost.Registration)
	if !ok {
		// A completed actor entry is a bounded receipt; the registry's operation
		// readback rebuilds the registration, as it does after a lost reply.
		readback, err := readArtifactRegistrationResult(h.artifacts, string(operationID), digest)
		if err != nil {
			return nil, err
		}
		if registration, ok = readback.(artifacthost.Registration); !ok {
			return nil, errors.New("workass_render_html registration receipt is invalid")
		}
	}
	return htmlRenderResult(registration, title)
}
