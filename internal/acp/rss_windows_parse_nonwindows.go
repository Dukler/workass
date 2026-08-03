//go:build !windows

package acp

import "strconv"

func pidString(pid int) string {
	return strconv.Itoa(pid)
}
