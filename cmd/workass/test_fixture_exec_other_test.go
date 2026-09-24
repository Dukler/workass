//go:build windows || plan9 || js || wasip1

package main

import (
	"errors"
)

func wireFixtureExecScript(_, _ []string) error {
	return errors.New("executable fixture sidecars are not supported on this platform")
}
