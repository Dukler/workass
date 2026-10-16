package scheduledmessage

import (
	"net"
	"os"
	"time"
)

// Probes answer whether a condition holds. A nil field uses the default.
type Probes struct {
	TCPUp      func(addr string) bool
	FileExists func(path string) bool
	PIDAlive   func(pid int) bool
}

func (p Probes) withDefaults() Probes {
	if p.TCPUp == nil {
		p.TCPUp = defaultTCPUp
	}
	if p.FileExists == nil {
		p.FileExists = defaultFileExists
	}
	if p.PIDAlive == nil {
		p.PIDAlive = defaultPIDAlive
	}
	return p
}

func defaultTCPUp(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func defaultFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
