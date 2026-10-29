package headlessupdate

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ServiceUnitEnv lets an operator name the unit explicitly when the daemon is
// not started by systemd itself (a wrapper, a container, a test harness).
const ServiceUnitEnv = "WORKASS_SERVICE_UNIT"

// FeedURLEnv points this daemon at another manifest. Private mirrors and the
// test harness use it; production leaves it unset and reads the GitHub feed.
const FeedURLEnv = "WORKASS_UPDATE_FEED_URL"

var unitNamePattern = regexp.MustCompile(`^[A-Za-z0-9@_.-]{1,96}\.service$`)

// Service is the `systemd --user` unit this daemon runs under, and the only
// restart authority the headless updater uses. Nothing here needs root: a user
// unit is restarted by the same user that owns the install.
type Service struct {
	Unit    string `json:"unit"`
	Restart string `json:"restart"`
	User    bool   `json:"user"`
}

// detectUnit reads the unit name out of this process's own cgroup. systemd
// exports INVOCATION_ID but not the unit name, and asking systemctl "what am
// I" needs the name first, so the cgroup is the only self-description a
// service actually has.
func detectUnit(env func(string) string, readFile func(string) ([]byte, error)) (string, bool, error) {
	if override := strings.TrimSpace(env(ServiceUnitEnv)); override != "" {
		if !unitNamePattern.MatchString(override) {
			return "", false, errors.New(ServiceUnitEnv + " must name one systemd .service unit")
		}
		return override, true, nil
	}
	raw, err := readFile("/proc/self/cgroup")
	if err != nil {
		return "", false, errors.New("this process is not managed by a systemd unit")
	}
	unit := ""
	user := false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 {
			continue
		}
		path := fields[2]
		if strings.Contains(path, "/user@") {
			user = true
		}
		for _, part := range strings.Split(path, "/") {
			if strings.HasSuffix(part, ".service") && unitNamePattern.MatchString(part) {
				unit = part
			}
		}
	}
	if unit == "" {
		return "", false, errors.New("this process is not managed by a systemd unit")
	}
	return unit, user, nil
}

// describeService resolves the unit and its Restart= policy. The policy is the
// whole activation mechanism: the daemon replaces its own files and then exits,
// and systemd is what brings the new binary up. A unit that is not configured
// to restart cannot be updated this way, and saying so is better than leaving
// a machine down.
func describeService(options resolvedOptions) (*Service, error) {
	unit, user, err := detectUnit(options.env, options.readFile)
	if err != nil {
		return nil, err
	}
	out, err := options.run("systemctl", "--user", "show", "--property=Restart", "--value", unit)
	if err != nil {
		return nil, errors.New("systemd --user is not reachable from this daemon: " + firstLine(err.Error()))
	}
	restart := strings.TrimSpace(string(out))
	if restart == "" || restart == "no" {
		return nil, errors.New("unit " + unit + " has Restart=" + orNone(restart) + "; add Restart=on-failure before a headless daemon can activate its own update")
	}
	return &Service{Unit: unit, Restart: restart, User: user}, nil
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return strings.TrimSpace(value[:index])
	}
	return strings.TrimSpace(value)
}

// writableDirectory proves the install root can actually be replaced before
// any byte is downloaded. A read-only or root-owned tree is a supported-false
// answer, never a half-applied update.
func writableDirectory(root string) error {
	probe, err := os.CreateTemp(root, ".workass-update-probe-")
	if err != nil {
		return err
	}
	name := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(name)
	return errors.Join(closeErr, removeErr)
}

func sameDirectory(left, right string) bool {
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	return leftResolved == rightResolved
}
