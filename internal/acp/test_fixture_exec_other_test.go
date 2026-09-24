//go:build windows || plan9 || js || wasip1

package acp

import "errors"

func fixtureExecScript(_, _ []string) error {
	return errors.New("executable fixture sidecars are not supported on this platform")
}
