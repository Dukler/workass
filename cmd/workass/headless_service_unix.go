//go:build !windows

package main

import "fmt"

func installHeadlessService(options headlessServiceOptions) error {
	return fmt.Errorf("automatic background service installation is no longer supported on this platform; open Workass or run --headless directly")
}
