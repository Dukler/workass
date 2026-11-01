package wire

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// A connection from this machine's loopback proves which machine is asking, not
// which page: every page in every local browser can reach the daemon, and neither
// a WebSocket nor a simple POST needs a CORS preflight. The Host and Origin a
// browser sends are what a page cannot choose, so they decide.

// viewOriginHeader is how the shell's view server names its own origin on a
// WebSocket it proxied, after verifying the page has exactly that origin. The
// proxy rewrites Host to the daemon's, so without it the daemon sees an Origin on
// some other loopback port and cannot tell the app's window from any other page.
// A browser cannot set a header on a WebSocket, and a local program that can
// already connects with no Origin at all.
const viewOriginHeader = "X-Workass-View-Origin"

// loopbackOrigin canonicalizes an http(s) origin on this machine's loopback, with
// its port made explicit, so that exact comparison means scheme, host AND port.
func loopbackOrigin(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	port := parsed.Port()
	switch parsed.Scheme {
	case "http":
		if port == "" {
			port = "80"
		}
	case "https":
		if port == "" {
			port = "443"
		}
	default:
		return "", false
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname != "localhost" {
		if ip := net.ParseIP(hostname); ip == nil || !ip.IsLoopback() {
			return "", false
		}
	}
	return parsed.Scheme + "://" + net.JoinHostPort(hostname, port), true
}

// requestOwnOrigin is the origin this request was addressed to. A foreign Host
// has none: to a DNS-rebinding page, its own origin and the Host it sends agree,
// which is exactly why agreement alone cannot be the rule.
//
// It comes from the Host rather than the listener's port so that a page served
// through an SSH tunnel to this daemon is still this daemon's own page. A browser
// sets Host from the address it dials, so a page elsewhere cannot claim one.
func requestOwnOrigin(r *http.Request) (string, bool) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return loopbackOrigin(scheme + "://" + strings.TrimSpace(r.Host))
}

// trustedLocalOrigin decides loopback auto-pairing: no Origin (the Workass CLI,
// native hosts, Go clients; a browser always sends one on a WebSocket), this
// daemon's own origin, or the view server's own origin, each compared exactly. A
// page on any other loopback port asks for approval like a LAN device.
func trustedLocalOrigin(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		return true
	}
	origin, ok := loopbackOrigin(raw)
	if !ok {
		return false
	}
	if own, ok := requestOwnOrigin(r); ok && origin == own {
		return true
	}
	// The stamp alone is not enough: a view server older than this rule stamps
	// whatever page it forwards, and names no origin.
	if strings.TrimSpace(r.Header.Get("X-Workass-Shell")) != "1" {
		return false
	}
	view, ok := loopbackOrigin(r.Header.Get(viewOriginHeader))
	return ok && origin == view
}

// LocalControlRequestAllowed guards the plain HTTP routes that act for this
// machine's owner (daemon shutdown, updates, the fleet join code). The Host must
// be loopback, so a rebinding page is out; an Origin must be this daemon's own,
// and fetch metadata must not say another site asked. Their real callers are the
// shell and the updater, which are Node clients with neither header, and the
// Settings sheet's same-origin <img>. A browser sends no Origin on that GET, so
// it passes through the view server with fetch metadata alone.
func LocalControlRequestAllowed(r *http.Request) bool {
	own, ok := requestOwnOrigin(r)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "", "same-origin", "none":
	default:
		return false
	}
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		return true
	}
	origin, ok := loopbackOrigin(raw)
	return ok && origin == own
}
