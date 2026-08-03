package main

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"workass/internal/fleet"
	"workass/internal/fleetqr"
)

// fleetQRPath serves the join code as an SVG so a sheet can show one with an
// <img> tag and never hold the key in its own scripts. The picture is the
// credential, which is exactly why it is drawn here rather than in a renderer.
const fleetQRPath = "/workass/fleet-qr.svg"

// newFleetQRHandler draws this machine's join code.
//
// Localhost only, matching fleet:reveal (internal/wire/fleet_keys.go): reading a
// fleet key is allowed on the machine that holds it and nowhere else, and an
// image of the key is the key. Serving it to the LAN would hand the fleet to
// anyone who could reach the port — which is every device this design exists to
// keep OUT until it has enrolled.
func newFleetQRHandler(keys *fleet.Store, port int, logf func(format string, args ...any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip == nil || !ip.IsLoopback() {
			http.Error(w, "a fleet key is only readable on the machine that holds it", http.StatusForbidden)
			return
		}
		if keys == nil {
			http.Error(w, "this daemon has no fleet key store", http.StatusNotFound)
			return
		}
		key, minted, err := keys.EnsureKey()
		if err != nil {
			http.Error(w, "fleet key unavailable", http.StatusInternalServerError)
			return
		}
		if minted && logf != nil {
			logf("[fleet] minted key %s to draw a join code", key.KeyID)
		}

		// The address the PHONE must reach, which is never the one this request
		// arrived on: this handler only ever answers loopback.
		requested := strings.TrimSpace(r.URL.Query().Get("host"))
		join := fleetqr.Join{Key: key.Secret, Name: hostname(), Port: port}
		if requested == "" {
			hosts := fleetqr.ReachableHosts()
			if len(hosts) == 0 {
				http.Error(w, "this machine has no address another device could reach", http.StatusConflict)
				return
			}
			join.Host = hosts[0]
		} else {
			join.Host = requested
			if splitHost, splitPort, splitErr := net.SplitHostPort(requested); splitErr == nil {
				parsedPort, portErr := strconv.Atoi(splitPort)
				if portErr != nil {
					http.Error(w, "that address has no usable port", http.StatusBadRequest)
					return
				}
				join.Host, join.Port = splitHost, parsedPort
			}
		}

		payload, err := fleetqr.BuildURL(join)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		svg, err := fleetqr.SVG(payload)
		if err != nil {
			http.Error(w, "could not draw the join code", http.StatusInternalServerError)
			return
		}
		// Loud in the record for the same reason fleet:reveal is: this is a moment
		// the secret leaves the daemon.
		if logf != nil {
			logf("[fleet] join code drawn for %s under key %s", join.Address(), key.KeyID)
		}
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		// Never cached anywhere: a rotated key must not be served from a disk
		// cache, and the credential should not outlive the sheet that showed it.
		w.Header().Set("Cache-Control", "no-store, private")
		w.Header().Set("Content-Length", strconv.Itoa(len(svg)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(svg)
	})
}
