package machinebook

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DiscoveryPort is the only port automatic managed-machine discovery probes.
// PORT-SPEC's network law fixes this at TCP 80 because that is the sole inbound
// port available on the managed Windows machine.
const DiscoveryPort = 80

const (
	defaultScanConcurrency = 32
	defaultProbeTimeout    = 450 * time.Millisecond
)

// Scanner discovers Workass daemons by probing /workass/health on TCP port 80
// across this host's private directly-connected LAN. It sends no multicast or
// broadcast packets and never scans a public address range.
type Scanner struct {
	Book         *Book
	Interval     time.Duration
	ProbeTimeout time.Duration
	Concurrency  int
	OnChange     func(Entry)
	Logf         func(format string, args ...any)

	// Candidates is a deterministic test seam. Production leaves it empty and
	// derives private candidates from active LAN interfaces.
	Candidates []string
}

// Run performs an immediate scan, then repeats until ctx ends.
func (s *Scanner) Run(ctx context.Context) error {
	if s == nil || s.Book == nil {
		return errors.New("port-80 scanner needs a machine book")
	}
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	s.scan(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.scan(ctx)
		}
	}
}

func (s *Scanner) scan(ctx context.Context) {
	candidates := append([]string(nil), s.Candidates...)
	if len(candidates) == 0 {
		candidates = privateLANPort80Candidates()
	}
	if len(candidates) == 0 {
		return
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

	jobs := make(chan string)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer workers.Done()
			for address := range jobs {
				probeCtx, cancel := context.WithTimeout(ctx, timeout)
				entry, changed, err := s.Book.Discover(probeCtx, address)
				cancel()
				if err != nil {
					continue
				}
				if changed {
					s.logf("[machines] auto-detected %s (%s) at %s over TCP 80", entry.Name, entry.MachineID, address)
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
			return
		case jobs <- candidate:
		}
	}
	close(jobs)
	workers.Wait()
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
	return scanCandidates(networks, DiscoveryPort)
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

func isPort80Address(address string) bool {
	_, port, err := net.SplitHostPort(strings.TrimSpace(address))
	return err == nil && port == strconv.Itoa(DiscoveryPort)
}
