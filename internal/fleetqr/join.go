// Package fleetqr turns a fleet key plus an address into the join payload a
// phone scanner reads, and draws it in the two forms a machine can actually
// show: an SVG for a screen, and coloured half-blocks for a terminal on a box
// that has no screen at all.
//
// The QR carries the key ITSELF, not a token standing for it — enrolment is a
// challenge-response under that key, so a pointer would need a second mechanism
// and the whole point of the fleet key is that there is only one. The
// consequence belongs on any surface that draws this: whoever photographs the
// code is in the fleet.
package fleetqr

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"workass/internal/fleet"
)

const (
	// Scheme and JoinPath spell workass://join. A custom scheme rather than an
	// https link because there is no domain and no DNS by design — a universal
	// link would need exactly the central infrastructure this fleet avoids.
	Scheme   = "workass"
	JoinPath = "join"

	// DefaultPort is omitted from the payload when it matches, so the common
	// code stays short. It is the daemon's own default, not a separate constant
	// to drift from it.
	DefaultPort = 8788
)

// Join is one scannable enrolment: where to reach a daemon, and the key that
// admits you to it.
type Join struct {
	// Host is a bare host or IP the SCANNING DEVICE can reach. Never loopback:
	// the phone resolving 127.0.0.1 would dial itself.
	Host string
	// Port is the daemon's port. Zero means DefaultPort.
	Port int
	// Key is the fleet key in canonical form.
	Key string
	// Name is cosmetic only. The phone replaces it with whatever
	// /workass/health reports, so it exists to make a code readable by a human
	// deciding which machine they are pointing at, nothing more.
	Name string
}

// BuildURL renders the payload. It refuses rather than guesses: a code built
// around a loopback address or a malformed key scans perfectly and then fails
// at enrolment, which is the worst place to discover it.
func BuildURL(join Join) (string, error) {
	host := strings.TrimSpace(join.Host)
	if host == "" {
		return "", fmt.Errorf("fleetqr: an address is required; a key alone cannot start a join")
	}
	if isLoopbackHost(host) {
		return "", fmt.Errorf("fleetqr: %q is loopback — the scanning device would dial itself", host)
	}
	key, err := fleet.NormalizeSecret(join.Key)
	if err != nil {
		return "", fmt.Errorf("fleetqr: %w", err)
	}
	port := join.Port
	if port == 0 {
		port = DefaultPort
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("fleetqr: port %d is out of range", port)
	}

	address := host
	if port != DefaultPort {
		address = net.JoinHostPort(host, strconv.Itoa(port))
	}
	// Built by hand rather than with url.Values.Encode, for one reason: Encode
	// percent-escapes the colon in host:port. That is correct and every real URL
	// parser undoes it, but a colon is legal unencoded in a query component, and
	// h=192.168.1.50:18788 is both shorter in the code and readable by a human
	// reading the payload printed under it. Only the cosmetic name can contain
	// characters that genuinely need escaping.
	payload := Scheme + "://" + JoinPath + "?h=" + address + "&k=" + key
	if name := clampName(join.Name); name != "" {
		payload += "&n=" + url.QueryEscape(name)
	}
	return payload, nil
}

// ParseURL is the daemon-side mirror of the phone's parser. It exists so a test
// can round-trip a built payload rather than assert against a string literal
// that would drift from the scanner without anyone noticing.
//
// Unknown query parameters are IGNORED, deliberately. That is the forward
// compatibility hinge for a payload that will be sitting in a phone in someone's
// pocket long after this code changes: a field added later must not turn every
// existing scanner into a hard failure.
func ParseURL(raw string) (Join, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Join{}, fmt.Errorf("fleetqr: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, Scheme) {
		return Join{}, fmt.Errorf("fleetqr: not a %s:// link", Scheme)
	}
	// workass://join?… parses "join" as the URL host, since there is no
	// authority to speak of.
	if !strings.EqualFold(strings.Trim(parsed.Host+parsed.Path, "/"), JoinPath) {
		return Join{}, fmt.Errorf("fleetqr: not a join link")
	}
	query := parsed.Query()

	address := strings.TrimSpace(query.Get("h"))
	if address == "" {
		return Join{}, fmt.Errorf("fleetqr: no address; a key alone cannot start a join")
	}
	host, port := address, DefaultPort
	if splitHost, splitPort, splitErr := net.SplitHostPort(address); splitErr == nil {
		parsedPort, portErr := strconv.Atoi(splitPort)
		if portErr != nil || parsedPort < 1 || parsedPort > 65535 {
			return Join{}, fmt.Errorf("fleetqr: %q has no usable port", address)
		}
		host, port = splitHost, parsedPort
	}
	if isLoopbackHost(host) {
		return Join{}, fmt.Errorf("fleetqr: %q is loopback — the scanning device would dial itself", host)
	}

	key, err := fleet.NormalizeSecret(query.Get("k"))
	if err != nil {
		return Join{}, fmt.Errorf("fleetqr: %w", err)
	}
	return Join{Host: host, Port: port, Key: key, Name: strings.TrimSpace(query.Get("n"))}, nil
}

// Address is the dialable form, always with an explicit port.
func (j Join) Address() string {
	port := j.Port
	if port == 0 {
		port = DefaultPort
	}
	return net.JoinHostPort(j.Host, strconv.Itoa(port))
}

// nameLimit caps the cosmetic name. Every byte in the payload makes the code
// denser, and a denser code is harder to photograph off a screen — so the one
// field that buys nothing is not allowed to cost that. The phone replaces this
// with whatever /workass/health reports anyway, so truncation loses nothing.
const nameLimit = 24

func clampName(name string) string {
	trimmed := strings.TrimSpace(name)
	runes := []rune(trimmed)
	if len(runes) <= nameLimit {
		return trimmed
	}
	return strings.TrimSpace(string(runes[:nameLimit]))
}

func isLoopbackHost(host string) bool {
	trimmed := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if trimmed == "" || trimmed == "localhost" || strings.HasSuffix(trimmed, ".localhost") {
		return true
	}
	if ip := net.ParseIP(trimmed); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

// Candidate is one address this machine answers on, and the interface it came
// from. The interface name is what separates the network the phone is actually
// on from the three or four that merely look like it.
type Candidate struct {
	Host  string
	Iface string
}

// ReachableHosts lists the addresses another device could dial, best first. A
// developer machine has several — a VPN tunnel, a tailnet, a VM bridge — and all
// of them are private IPv4 addresses that look perfectly plausible. Picking the
// wrong one yields a code that scans and then connects to nothing, so the whole
// list is returned and the ordering carries the judgment.
func ReachableHosts() []string {
	candidates := ReachableCandidates()
	hosts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		hosts = append(hosts, candidate.Host)
	}
	return hosts
}

// ReachableCandidates is ReachableHosts with the interface each address came
// from, for a surface that wants to say "en0" next to the choice.
func ReachableCandidates() []Candidate {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var found []Candidate
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			network, ok := addr.(*net.IPNet)
			if !ok || network.IP == nil {
				continue
			}
			ip := network.IP.To4()
			// IPv4 only: this is typed or scanned by a human on a home network,
			// and a link-local IPv6 address carries a zone suffix that means
			// nothing on the other device.
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			found = append(found, Candidate{Host: ip.String(), Iface: iface.Name})
		}
	}
	return RankCandidates(found)
}

// RankCandidates orders addresses by how likely the phone is to be on that
// network. Split out from the enumeration so the judgment is testable without a
// particular machine's interfaces.
//
// The ordering is by interface CLASS, not by address range, because every one of
// these is a private address: a VPN on 10.x, a tailnet on 100.64/10 and a VM
// bridge on 192.168.64/24 are indistinguishable from the LAN by IP alone. The
// name is the only honest signal available without a routing table.
func RankCandidates(candidates []Candidate) []Candidate {
	ranked := make([]Candidate, len(candidates))
	copy(ranked, candidates)
	sort.SliceStable(ranked, func(a, b int) bool {
		rankA, rankB := interfaceRank(ranked[a]), interfaceRank(ranked[b])
		if rankA != rankB {
			return rankA < rankB
		}
		return ranked[a].Host < ranked[b].Host
	})
	return ranked
}

func interfaceRank(candidate Candidate) int {
	name := strings.ToLower(candidate.Iface)
	switch {
	// Tunnels and virtual switches last. A code built on one of these reaches a
	// device that joined the same VPN, which the phone standing next to you has
	// not.
	case hasAnyPrefix(name, "utun", "tun", "tap", "ppp", "wg", "ipsec", "gpd", "zt"):
		return 3
	case hasAnyPrefix(name, "bridge", "vmnet", "vnic", "docker", "veth", "virbr", "vboxnet", "awdl", "llw", "ap"):
		return 3
	// Ordinary wired and wireless interfaces: en0 on macOS, eth0/wlan0/enp*/wlp*
	// on Linux, and Ethernet/Wi-Fi on Windows.
	case hasAnyPrefix(name, "en", "eth", "wlan", "wl", "wi-fi", "wifi", "ethernet"):
		if candidate.IsPrivate() {
			return 0
		}
		return 1
	default:
		return 2
	}
}

// IsPrivate reports whether the address is in RFC1918 space, which on a home
// network is where the phone is.
func (c Candidate) IsPrivate() bool {
	ip := net.ParseIP(c.Host)
	return ip != nil && ip.IsPrivate()
}

func hasAnyPrefix(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
