package machinebook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DiscoveryPort is the port every sweep probes. PORT-SPEC's network note fixes it
// at TCP 80 because that is the sole inbound port available on a managed Windows
// machine.
const DiscoveryPort = 80

// DaemonPort is where macOS and Linux daemons listen by default. A sweep of port
// 80 alone can never find them (owner amendment 2026-11-01).
const DaemonPort = 8788

const (
	defaultScanConcurrency = 32
	defaultProbeTimeout    = 450 * time.Millisecond
	maxStableScanInterval  = time.Minute
)

// sweepSettle is how recent a finished sweep must be for a caller that waited
// behind it to reuse its answer rather than knock on every address again.
const sweepSettle = 5 * time.Second

// Scanner discovers Workass daemons by probing /workass/health across this host's
// private directly-connected LAN. It sends no multicast or broadcast packets and
// never scans a public address range.
type Scanner struct {
	Book         *Book
	Interval     time.Duration
	ProbeTimeout time.Duration
	Concurrency  int
	OnChange     func(Entry)
	Logf         func(format string, args ...any)

	// Ports are the ports each host is probed on. Empty means DiscoveryPort.
	Ports []int
	// Gate decides whether Run may sweep right now. Nil always may. It never
	// applies to SweepNow, which is a person or an agent asking.
	Gate func(context.Context) bool

	sweep     sync.Mutex
	lastSweep time.Time

	// Candidates is a deterministic test seam. Production leaves it empty and
	// derives private candidates from active LAN interfaces.
	Candidates []string
}

// Run keeps discovering new machines while backing off full subnet sweeps on
// a stable network. Cheap interface checks and known-machine liveness keep
// their original cadence; a topology change starts a fresh sweep immediately.
func (s *Scanner) Run(ctx context.Context) error {
	if s == nil || s.Book == nil {
		return errors.New("port-80 scanner needs a machine book")
	}
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	schedule := scanSchedule{base: interval}
	step := func() {
		candidates := s.candidates()
		if schedule.due(time.Now(), strings.Join(candidates, "\n")) {
			changed := false
			if s.Gate == nil || s.Gate(ctx) {
				changed = s.locked(ctx, candidates)
			}
			schedule.finished(time.Now(), changed)
		}
	}
	for {
		step()
		// Wake for either the next interface check or the exact scan deadline;
		// a sweep's duration must not round a 60-second delay up to 70 seconds.
		timer := time.NewTimer(max(time.Millisecond, min(interval, time.Until(schedule.next))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

type scanSchedule struct {
	base, delay time.Duration
	next        time.Time
	topology    string
}

func (s *scanSchedule) due(now time.Time, topology string) bool {
	if topology != s.topology {
		s.topology, s.delay, s.next = topology, 0, time.Time{}
	}
	return !now.Before(s.next)
}

func (s *scanSchedule) finished(now time.Time, changed bool) {
	if changed || s.delay == 0 {
		s.delay = s.base
	} else {
		s.delay = min(s.delay*2, max(s.base, maxStableScanInterval))
	}
	s.next = now.Add(s.delay)
}

func (s *Scanner) candidates() []string {
	candidates := append([]string(nil), s.Candidates...)
	if len(candidates) == 0 {
		networks := privateLANNetworks()
		ports := s.Ports
		if len(ports) == 0 {
			ports = []int{DiscoveryPort}
		}
		for _, port := range ports {
			candidates = append(candidates, scanCandidates(networks, port)...)
		}
	}
	sort.Strings(candidates)
	return candidates
}

// SweepNow sweeps immediately, whatever Gate says, and reports whether the book
// changed. One sweep runs at a time. A caller that waited behind another sweep
// reuses that answer instead of probing every address a second time.
func (s *Scanner) SweepNow(ctx context.Context) bool {
	if s == nil || s.Book == nil {
		return false
	}
	started := time.Now()
	s.sweep.Lock()
	defer s.sweep.Unlock()
	if s.lastSweep.After(started) && time.Since(s.lastSweep) < sweepSettle {
		return false
	}
	changed := s.scanCandidates(ctx, s.candidates())
	s.lastSweep = time.Now()
	return changed
}

func (s *Scanner) locked(ctx context.Context, candidates []string) bool {
	s.sweep.Lock()
	defer s.sweep.Unlock()
	changed := s.scanCandidates(ctx, candidates)
	s.lastSweep = time.Now()
	return changed
}

func (s *Scanner) scan(ctx context.Context) bool {
	return s.scanCandidates(ctx, s.candidates())
}

func (s *Scanner) scanCandidates(ctx context.Context, candidates []string) bool {
	if len(candidates) == 0 {
		return false
	}
	concurrency := s.Concurrency
	if concurrency <= 0 {
		concurrency = defaultScanConcurrency
	}
	if concurrency > len(candidates) {
		concurrency = len(candidates)
	}
	timeout := s.ProbeTimeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	probeClient, probeTransport := scanProbeClient(s.Book, timeout)
	defer probeTransport.CloseIdleConnections()
	probe := func(probeCtx context.Context, address string) (Card, error) {
		return probeWithClient(probeCtx, address, probeClient)
	}

	jobs := make(chan string)
	var discovered atomic.Bool
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer workers.Done()
			for address := range jobs {
				probeCtx, cancel := context.WithTimeout(ctx, timeout)
				entry, changed, err := s.Book.discover(probeCtx, address, probe)
				cancel()
				if err != nil {
					continue
				}
				if changed {
					discovered.Store(true)
					s.logf("[machines] auto-detected %s (%s) at %s", entry.Name, entry.MachineID, address)
					if s.OnChange != nil {
						s.OnChange(entry)
					}
				}
			}
		}()
	}
	for _, candidate := range candidates {
		select {
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return discovered.Load()
		case jobs <- candidate:
		}
	}
	close(jobs)
	workers.Wait()
	return discovered.Load()
}

// scanProbeClient gives one LAN sweep its own transport. A request context is
// not enough to bound a Transport dial: after a request is cancelled, Go may
// keep that dial alive for connection reuse. On a /24 sweep that left hundreds
// of TCP-80 SYNs in flight until the default dialer's much longer timeout.
// Bound the dial itself and disable pooling because each candidate is normally
// contacted only once per scan.
func scanProbeClient(book *Book, timeout time.Duration) (*http.Client, *http.Transport) {
	transport := book.probeTransport.Clone()
	transport.DialContext = boundedDialContext(transport.DialContext, timeout)
	if transport.DialTLSContext != nil {
		transport.DialTLSContext = boundedDialContext(transport.DialTLSContext, timeout)
	}
	transport.DisableKeepAlives = true

	client := *book.client
	client.Transport = transport
	if client.Timeout <= 0 || client.Timeout > timeout {
		client.Timeout = timeout
	}
	return &client, transport
}

func boundedDialContext(dial func(context.Context, string, string) (net.Conn, error), timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return dial(dialCtx, network, address)
	}
}

func (s *Scanner) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// privateLANPort80Candidates returns hosts only from private, directly attached
// IPv4 LANs. Each interface is capped to its local /24 when the configured
// prefix is broader, preventing an accidental sweep of a corporate /16.
func privateLANPort80Candidates() []string {
	return scanCandidates(privateLANNetworks(), DiscoveryPort)
}

// privateLANNetworks lists the private IPv4 networks this host is directly
// attached to. VPN and other point-to-point interfaces are not among them.
func privateLANNetworks() []*net.IPNet {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var networks []*net.IPNet
	for _, iface := range interfaces {
		if !scannableInterface(iface) {
			continue
		}
		addrs, addrErr := iface.Addrs()
		if addrErr != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || !ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				continue
			}
			networks = append(networks, &net.IPNet{IP: append(net.IP(nil), ip...), Mask: append(net.IPMask(nil), ipnet.Mask...)})
		}
	}
	return networks
}

func scannableInterface(iface net.Interface) bool {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagBroadcast == 0 {
		return false
	}
	return iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) == 0
}

func scanCandidates(networks []*net.IPNet, port int) []string {
	if port <= 0 {
		port = DiscoveryPort
	}
	self := make(map[string]struct{}, len(networks))
	for _, network := range networks {
		if network != nil && network.IP.To4() != nil {
			self[network.IP.To4().String()] = struct{}{}
		}
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(networks)*253)
	for _, network := range networks {
		if network == nil {
			continue
		}
		ip := network.IP.To4()
		if ip == nil || !ip.IsPrivate() {
			continue
		}
		ones, bits := network.Mask.Size()
		if bits != 32 || ones < 0 || ones > 30 {
			continue
		}
		if ones < 24 {
			ones = 24
		}
		mask := net.CIDRMask(ones, 32)
		base := make(net.IP, net.IPv4len)
		for i := range base {
			base[i] = ip[i] & mask[i]
		}
		hostBits := uint(32 - ones)
		last := uint32(1)<<hostBits - 1
		baseInt := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
		for offset := uint32(1); offset < last; offset++ {
			value := baseInt + offset
			candidateIP := net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value)).To4()
			plain := candidateIP.String()
			if _, own := self[plain]; own {
				continue
			}
			address := net.JoinHostPort(plain, strconv.Itoa(port))
			if _, duplicate := seen[address]; duplicate {
				continue
			}
			seen[address] = struct{}{}
			out = append(out, address)
		}
	}
	return out
}

// isSweepAddress reports whether automatic discovery may probe this address.
func isSweepAddress(address string) bool {
	_, port, err := net.SplitHostPort(strings.TrimSpace(address))
	return err == nil && (port == strconv.Itoa(DiscoveryPort) || port == strconv.Itoa(DaemonPort))
}

func isPort80Address(address string) bool {
	_, port, err := net.SplitHostPort(strings.TrimSpace(address))
	return err == nil && port == strconv.Itoa(DiscoveryPort)
}
