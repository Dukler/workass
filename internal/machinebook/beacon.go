package machinebook

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The beacon's group and port. Multicast rather than broadcast for two reasons:
// it is scoped to hosts that asked to hear it, and ListenMulticastUDP is the
// one stdlib call that sets SO_REUSEADDR — without which a second daemon on the
// same machine could not listen at all, and the first acceptance gate is two
// daemons on one Mac finding each other.
const (
	// GroupIP is administratively scoped (239.0.0.0/8): it does not leave the
	// local network, and no router will forward it anywhere.
	GroupIP = "239.87.87.88"
	// GroupPort is UDP, and shares a number with nothing — the daemon's own
	// 8788 is TCP.
	GroupPort = 48788
)

// DefaultInterval is how often a daemon says it is here. It also bounds how
// long a machine that died looks alive, since the refresh loop runs on the same
// clock.
const DefaultInterval = 10 * time.Second

// maxNewPerInterval bounds how many unknown machines one interval may make us
// probe. An announcement is unauthenticated UDP that anyone on the network can
// send, and each unknown one costs a TCP connection; without a ceiling, a
// stream of invented machine ids turns this daemon into someone else's traffic
// generator. Clamping is logged, never silent.
const maxNewPerInterval = 8

// announcement is deliberately the smallest thing that answers "should I look?"
// — an id to recognise, a port to reach, and a name so a packet capture reads.
// Nothing here is trusted: the address comes from the packet's source, and
// everything else is re-read from the machine's own health document afterwards.
type announcement struct {
	App       string `json:"app"`
	MachineID string `json:"machineId"`
	Name      string `json:"name"`
	Port      int    `json:"port"`
	At        string `json:"at"`
}

// Beacon listens for other daemons and optionally announces this one.
type Beacon struct {
	Book      *Book
	MachineID string
	Name      string
	Port      int
	Interval  time.Duration
	OnChange  func(Entry)
	Logf      func(format string, args ...any)

	// Group overrides the multicast address, and exists so that a test can put
	// its announcements somewhere real daemons are not listening. A beacon in a
	// test still puts real packets on a real network — without this, running
	// the suite writes invented machines into the book of every daemon on the
	// LAN, including someone else's.
	Group string
	// ReceiveOnly joins the multicast group and records reachable announcers
	// without advertising this daemon. A loopback-bound controller uses this
	// mode to discover LAN-reachable machines without exposing itself.
	ReceiveOnly bool

	mu         sync.Mutex
	listeners  map[int]*net.UDPConn
	announceFn func(*net.UDPAddr)
}

// Run listens until ctx ends and, unless ReceiveOnly is set, announces on a
// ticker. Receive-only mode is safe for a loopback-bound daemon.
func (b *Beacon) Run(ctx context.Context) error {
	if strings.TrimSpace(b.MachineID) == "" {
		return errors.New("beacon needs a machine id")
	}
	if b.Port <= 0 {
		return errors.New("beacon needs the daemon's port")
	}
	interval := b.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	b.listeners = map[int]*net.UDPConn{}
	defer b.closeListeners()

	group := &net.UDPAddr{IP: net.ParseIP(GroupIP), Port: GroupPort}
	if custom := strings.TrimSpace(b.Group); custom != "" {
		resolved, resolveErr := net.ResolveUDPAddr("udp4", custom)
		if resolveErr != nil {
			return resolveErr
		}
		group = resolved
	}
	b.refreshListeners(ctx, group)
	if !b.ReceiveOnly {
		b.sendAnnouncement(group)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Re-enumerated every tick rather than once at start: Wi-Fi comes
			// up, VPNs attach, docks appear. One packet per interface per
			// interval is cheap enough to pay for never needing a restart.
			b.refreshListeners(ctx, group)
			if !b.ReceiveOnly {
				b.sendAnnouncement(group)
			}
		}
	}
}

func (b *Beacon) sendAnnouncement(group *net.UDPAddr) {
	if b.announceFn != nil {
		b.announceFn(group)
		return
	}
	b.announce(group)
}

// announce sends one packet per usable interface. The kernel picks the egress
// interface for a multicast destination, so the send is bound to each
// interface's own address in turn to reach every network this host is on.
func (b *Beacon) announce(group *net.UDPAddr) {
	payload, err := json.Marshal(announcement{
		App:       "workass",
		MachineID: b.MachineID,
		Name:      b.Name,
		Port:      b.Port,
		At:        time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	for _, addr := range multicastAddrs() {
		conn, dialErr := net.DialUDP("udp4", &net.UDPAddr{IP: addr.IP}, group)
		if dialErr != nil {
			continue
		}
		_, _ = conn.Write(payload)
		conn.Close()
	}
}

// refreshListeners joins the group on every interface that can carry it, and
// starts a reader for each newly joined one.
func (b *Beacon) refreshListeners(ctx context.Context, group *net.UDPAddr) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return
	}
	for _, iface := range interfaces {
		if !usableInterface(iface) {
			continue
		}
		b.mu.Lock()
		_, joined := b.listeners[iface.Index]
		b.mu.Unlock()
		if joined {
			continue
		}
		conn, listenErr := net.ListenMulticastUDP("udp4", &iface, group)
		if listenErr != nil {
			continue
		}
		b.mu.Lock()
		b.listeners[iface.Index] = conn
		b.mu.Unlock()
		go b.read(ctx, conn, iface.Name)
	}
}

func (b *Beacon) read(ctx context.Context, conn *net.UDPConn, ifaceName string) {
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buffer := make([]byte, 2048)
	window := time.Now()
	probed := 0
	for {
		n, src, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		var heard announcement
		if json.Unmarshal(buffer[:n], &heard) != nil {
			continue
		}
		if heard.App != "workass" || heard.Port <= 0 || strings.TrimSpace(heard.MachineID) == "" {
			continue
		}
		if heard.MachineID == b.MachineID || src == nil {
			continue
		}
		if elapsed := time.Since(window); elapsed >= DefaultInterval {
			window = time.Now()
			probed = 0
		}
		// The address is the packet's source, never a field inside it. A
		// payload-supplied address would let anyone on this network point us
		// at a third party and have us connect to it.
		address := net.JoinHostPort(src.IP.String(), strconv.Itoa(heard.Port))
		if !reserveProbe(b.Book.sightingNeedsProbe(heard.MachineID, address), &probed) {
			b.logf("[machines] %s: too many new announcements this interval, ignoring the rest", ifaceName)
			continue
		}
		entry, changed, sightErr := b.Book.Sighted(ctx, heard.MachineID, address)
		if sightErr != nil {
			if !errors.Is(sightErr, ErrSelf) {
				b.logf("[machines] %s announced at %s but did not answer: %v", heard.MachineID, address, sightErr)
			}
			continue
		}
		if changed {
			b.logf("[machines] %s (%s) at %s", entry.Name, entry.MachineID, address)
			if b.OnChange != nil {
				b.OnChange(entry)
			}
		}
	}
}

func reserveProbe(required bool, used *int) bool {
	if !required {
		return true
	}
	if *used >= maxNewPerInterval {
		return false
	}
	*used++
	return true
}

func (b *Beacon) closeListeners() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for index, conn := range b.listeners {
		conn.Close()
		delete(b.listeners, index)
	}
}

func (b *Beacon) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

// usableInterface decides where this daemon is willing to be heard.
//
// Broadcast-capable and not point-to-point, which is the portable way to say
// "a local network" rather than "a tunnel": Ethernet, Wi-Fi and VM bridges
// carry broadcast, while WireGuard and Tailscale interfaces are point-to-point
// on macOS and non-broadcast on Linux. That distinction is the whole reason
// this check exists — a VPN is a network the user joined for other purposes,
// and announcing onto it would put this daemon's presence in front of every
// machine on it. Riding a VPN deliberately is a later feature; leaking onto one
// today is not the way to get there.
//
// Loopback is out for a different reason: a daemon that only talks to itself
// has nothing to announce.
func usableInterface(iface net.Interface) bool {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
		return false
	}
	if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
		return false
	}
	return iface.Flags&net.FlagBroadcast != 0
}

// multicastAddrs lists this host's IPv4 addresses worth announcing from.
func multicastAddrs() []*net.IPNet {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := []*net.IPNet{}
	for _, iface := range interfaces {
		if !usableInterface(iface) {
			continue
		}
		addrs, addrErr := iface.Addrs()
		if addrErr != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ipnet)
		}
	}
	return out
}
