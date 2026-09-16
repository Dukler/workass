//go:build !windows

package appinstall

import "errors"

func retryableRename(error) bool { return false }

func supportedPlatform() error { return errors.New("install-update is available only on Windows") }
func platformOperations(Plan) (operations, func(), error) {
	return operations{}, nil, supportedPlatform()
}
