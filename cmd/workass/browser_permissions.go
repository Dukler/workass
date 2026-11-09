package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"workass/internal/acp"
	providercontract "workass/internal/provider"
)

// The owner's rules for the agent browser (Ajustes · Permisos · Navegador del
// agente), held at the tool boundary before a call reaches the shell:
//
//   - Navegar: block refuses every browser tool but close. Ask asks the owner,
//     on the chat's question card, the first time a chat's agent opens a site by
//     address (open with a url, navigate); a yes holds for that site in that chat
//     until the daemon restarts. Links and scripts inside a page it already has
//     are part of using that page.
//   - Subir archivos: block refuses each upload; ask asks for each one.
//   - Descargar archivos: the shell enforces it (browser-actions.js takeDownload),
//     because a download starts in the page after the action that caused it has
//     returned. Every agent action carries the current rule there.
//
// A card needs a turn of the calling chat to hang off; a subagent cannot ask the
// user at all (permission_ceiling.go), so either gets a refusal saying why.

const (
	browserBlockedCode    = "browser_blocked"
	browserDeniedCode     = "browser_denied"
	browserCannotAskCode  = "browser_cannot_ask"
	browserPermissionID   = "browser_permission"
	browserPermissionYes  = "allow"
	browserPermissionNo   = "deny"
	browserDownloadPolicy = "downloadPolicy"
)

// browserSiteApprovals remembers the sites the owner let each chat's agent open.
type browserSiteApprovals struct {
	mu    sync.Mutex
	sites map[string]map[string]bool
}

func newBrowserSiteApprovals() *browserSiteApprovals {
	return &browserSiteApprovals{sites: map[string]map[string]bool{}}
}

func (a *browserSiteApprovals) allowed(chatID, site string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sites[chatID][site]
}

func (a *browserSiteApprovals) allow(chatID, site string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sites[chatID] == nil {
		a.sites[chatID] = map[string]bool{}
	}
	a.sites[chatID][site] = true
}

// browserSite is what a navigation is asked about: the host, or the scheme for a
// page with none (about:, data:).
func browserSite(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if host := strings.ToLower(parsed.Hostname()); host != "" {
		return host
	}
	if parsed.Scheme != "" {
		return strings.ToLower(parsed.Scheme) + ":"
	}
	return ""
}

func browserOpensSite(prepared preparedBrowserMCPCall) (string, bool) {
	switch prepared.Method {
	case "browser.open", "browser.navigate":
		address := strings.TrimSpace(browserString(prepared.Params["url"]))
		if address == "" {
			return "", false
		}
		return address, true
	}
	return "", false
}

// admitBrowserCall applies the owner's browser rules to one call. A nil result
// lets the call through; anything else is the tool result the agent reads.
func (h *workassToolHandler) admitBrowserCall(ctx context.Context, prepared preparedBrowserMCPCall,
	operationID providercontract.OperationID, ownerKey, chatID, tabID string) map[string]any {
	rules := h.agentControl.agentPermissions()
	if rules.BrowserNavigate == browserPolicyBlock && prepared.Method != "browser.close" {
		return browserMCPErrorResult(browserBlockedCode + ": the user turned the agent browser off in Workass settings (Permissions). Nothing was opened or read. Do not retry; tell the user what you needed the browser for")
	}
	if prepared.Method == "browser.upload" {
		switch rules.BrowserUpload {
		case browserPolicyBlock:
			return browserMCPErrorResult(browserBlockedCode + ": the user blocks agent browser uploads in Workass settings (Permissions). Nothing was uploaded. Do not retry; tell the user which file the page needs")
		case browserPolicyAsk:
			files := browserUploadNames(prepared.Params["paths"])
			site := h.browserTabSite(chatID, prepared.Params["tabId"])
			question := "El agente quiere subir " + files + " a una página"
			if site != "" {
				question += " de " + site
			}
			// The card names the page's site; what it approves is this tab and these
			// files, so a retry after the page moved still reads the same answer.
			subject := toString(prepared.Params["tabId"]) + "\x00" + toString(prepared.Params["paths"])
			return h.askBrowserPermission(ctx, operationID, ownerKey, chatID, tabID, "upload", subject,
				question+". ¿Lo dejás?", "uploading "+files)
		}
	}
	if address, opens := browserOpensSite(prepared); opens && rules.BrowserNavigate == browserPolicyAsk {
		site := browserSite(address)
		if site == "" || h.browserApprovals.allowed(chatID, site) {
			// An address with no site is the shell's to refuse in its own words.
			return nil
		}
		refusal := h.askBrowserPermission(ctx, operationID, ownerKey, chatID, tabID, "navigate", site,
			"El agente quiere abrir "+site+" en el navegador. Si lo dejás, puede seguir navegando ese sitio en este chat. ¿Lo dejás?",
			"opening "+site)
		if refusal == nil {
			h.browserApprovals.allow(chatID, site)
		}
		return refusal
	}
	return nil
}

// withBrowserDownloadRule hands the owner's download rule to the shell with an
// agent action. It is added after the request digest, which names the action and
// not the rule it ran under.
func (h *workassToolHandler) withBrowserDownloadRule(params map[string]any) {
	params[browserDownloadPolicy] = h.agentControl.agentPermissions().BrowserDownload
}

// askBrowserPermission asks the owner on the chat's question card and waits, the
// way the permission ceiling does: durable by operation id, so a retry reads the
// answer and never opens a second card.
func (h *workassToolHandler) askBrowserPermission(ctx context.Context, operationID providercontract.OperationID,
	ownerKey, chatID, tabID, action, subject, question, doing string) map[string]any {
	if subagentID, child := ctx.Value(permissionChildKey{}).(string); child && subagentID != "" {
		return browserMCPErrorResult(fmt.Sprintf("%s: the user set Workass to ask before the agent browser is %s, and a subagent cannot ask the user. Report what you need; your parent agent can ask", browserCannotAskCode, doing))
	}
	if h.agentControl == nil || h.agentControl.chats == nil || h.agentControl.chats.providerChats == nil {
		return browserMCPErrorResult(fmt.Sprintf("%s: the user set Workass to ask before the agent browser is %s, and there is no chat here to ask in", browserCannotAskCode, doing))
	}
	if runes := []rune(question); len(runes) > 400 {
		question = string(runes[:399]) + "…"
	}
	card, err := parseWorkassQuestion(map[string]any{
		"operation_id": string(derivedOperationID("browser-permission:", operationID, "")),
		"question_id":  browserPermissionID,
		"header":       "Navegador",
		"question":     question,
		"options": []any{
			map[string]any{"id": browserPermissionYes, "label": "Permitir"},
			map[string]any{"id": browserPermissionNo, "label": "No"},
		},
	})
	if err != nil {
		return browserMCPErrorResult(err.Error())
	}
	raw, _ := json.Marshal(map[string]string{"action": action, "chatId": chatID, "subject": subject})
	card.Digest = fmt.Sprintf("%x", sha256.Sum256(append([]byte("workass-browser-permission-v1\x00"), raw...)))
	value, err := h.agentControl.chats.providerChats.executeAgentQuestion(ctx, h.manager, ownerKey, tabID, chatID, card)
	if err != nil {
		if acp.IsNoRunningQuestionCaller(err) {
			return browserMCPErrorResult(fmt.Sprintf("%s: the user set Workass to ask before the agent browser is %s, and no turn is running here to show them a card. Ask the user in your reply", browserCannotAskCode, doing))
		}
		return browserMCPErrorResult(err.Error())
	}
	result, _ := value.(workassQuestionResult)
	if result.Status == "answered" {
		for _, selected := range result.SelectedOptions {
			if selected.ID == browserPermissionYes {
				return nil
			}
		}
	}
	status := result.Status
	if status == "" || status == "answered" {
		status = "denied"
	}
	return browserMCPErrorResult(fmt.Sprintf("%s: the user was asked about the agent browser %s and the card ended as %s. Nothing was done. A retry with this operation_id reads the same answer; a new operation_id asks again", browserDeniedCode, doing, status))
}

// browserUploadNames names the files an upload would send, by base name only.
func browserUploadNames(value any) string {
	items, _ := value.([]any)
	names := make([]string, 0, len(items))
	for _, item := range items {
		if name := filepath.Base(strings.TrimSpace(toString(item))); name != "" && name != "." && name != string(filepath.Separator) {
			names = append(names, acp.SanitizeDisplayText(name, 80))
		}
	}
	switch len(names) {
	case 0:
		return "un archivo"
	case 1:
		return names[0]
	default:
		return names[0] + " y " + strconv.Itoa(len(names)-1) + " más"
	}
}

// browserTabSite is the site the chat's tab shows, for the upload card. A tab the
// shell cannot name leaves it out rather than holding the card.
func (h *workassToolHandler) browserTabSite(chatID string, tabID any) string {
	if strings.TrimSpace(h.browserControlFile) == "" {
		return ""
	}
	listed, err := invokeBrowserControl(h.browserControlFile, "browser.list", map[string]any{"chatId": chatID}, h.browserClient)
	if err != nil {
		return ""
	}
	want, _ := browserFiniteNumber(tabID)
	var fallback string
	for _, raw := range anySlice(mapFromAnyMain(listed)["tabs"]) {
		tab := mapFromAnyMain(raw)
		id, _ := browserFiniteNumber(tab["id"])
		site := browserSite(browserString(tab["url"]))
		if tabID != nil && id == want {
			return site
		}
		if tab["active"] == true || fallback == "" {
			fallback = site
		}
	}
	if tabID != nil {
		return ""
	}
	return fallback
}
