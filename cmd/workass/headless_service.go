package main

import "strings"

type headlessServiceOptions struct {
	Executable string
	StateDir   string
	Port       int
	Bind       string
	Profile    string
}

// serviceName is deliberately stable across upgrades so installing a new
// portable bundle replaces the existing user service instead of creating a
// second daemon that races for the same port.
func serviceName() string { return "Workass Headless" }

func trimServiceOutput(output []byte) string {
	const max = 512
	text := strings.TrimSpace(string(output))
	if len(text) > max {
		return text[:max]
	}
	return text
}
