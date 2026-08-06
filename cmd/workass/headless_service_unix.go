//go:build !windows

package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func installHeadlessService(options headlessServiceOptions) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("headless service installation is supported on macOS and Windows only")
	}
	if options.Executable == "" {
		return fmt.Errorf("the executable path is unavailable")
	}
	if options.Port < 1 || options.Port > 65535 {
		return fmt.Errorf("invalid service port %d", options.Port)
	}
	if options.Bind != "localhost" && options.Bind != "lan" {
		return fmt.Errorf("invalid service bind %q", options.Bind)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	current, err := user.Current()
	if err != nil {
		return err
	}
	label := "com.workass.daemon.headless"
	launchAgents := filepath.Join(home, "Library", "LaunchAgents")
	logRoot := filepath.Join(home, "Library", "Logs", "Workass")
	plistPath := filepath.Join(launchAgents, label+".plist")
	if err := os.MkdirAll(launchAgents, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(logRoot, 0o700); err != nil {
		return err
	}
	env := map[string]string{
		"HOME":              home,
		"PATH":              os.Getenv("PATH"),
		"WORKASS_PROFILE":   options.Profile,
		"WORKASS_DATA_ROOT": filepath.Dir(options.StateDir),
	}
	args := []string{options.Executable, "--prod", "--headless", "--state-dir", options.StateDir, "--port", strconv.Itoa(options.Port), "--bind", options.Bind}
	plist := launchdServicePlist(label, args, filepath.Dir(options.Executable), logRoot, env)
	incoming := plistPath + ".incoming-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(incoming, []byte(plist), 0o600); err != nil {
		return err
	}
	if err := os.Rename(incoming, plistPath); err != nil {
		_ = os.Remove(incoming)
		return err
	}
	domain := "gui/" + current.Uid
	_ = exec.Command("launchctl", "bootout", domain, plistPath).Run()
	if output, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w (%s)", err, trimServiceOutput(output))
	}
	if output, err := exec.Command("launchctl", "enable", domain+"/"+label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl enable: %w (%s)", err, trimServiceOutput(output))
	}
	if output, err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart: %w (%s)", err, trimServiceOutput(output))
	}
	return nil
}

type launchdPlist struct {
	XMLName xml.Name    `xml:"plist"`
	Version string      `xml:"version,attr"`
	Dict    launchdDict `xml:"dict"`
}

type launchdDict struct {
	Label                string     `xml:"Label"`
	ProgramArguments     []string   `xml:"ProgramArguments>string"`
	WorkingDirectory     string     `xml:"WorkingDirectory"`
	RunAtLoad            bool       `xml:"RunAtLoad"`
	KeepAlive            bool       `xml:"KeepAlive"`
	StandardOutPath      string     `xml:"StandardOutPath"`
	StandardErrorPath    string     `xml:"StandardErrorPath"`
	EnvironmentVariables launchdEnv `xml:"EnvironmentVariables"`
}

type launchdEnv struct {
	Values []launchdEnvValue `xml:"-"`
}

type launchdEnvValue struct {
	Key   string
	Value string
}

func launchdServicePlist(label string, args []string, workingDir, logRoot string, env map[string]string) string {
	// encoding/xml cannot express a dynamic dictionary without custom marshal
	// methods, so keep the shape explicit and escape every user-controlled path.
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\"><dict>")
	writeXMLKeyValue(&b, "Label", label)
	b.WriteString("<key>ProgramArguments</key><array>")
	for _, arg := range args {
		b.WriteString("<string>")
		b.WriteString(xmlEscapeString(arg))
		b.WriteString("</string>")
	}
	b.WriteString("</array>")
	writeXMLKeyValue(&b, "WorkingDirectory", workingDir)
	b.WriteString("<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>")
	writeXMLKeyValue(&b, "StandardOutPath", filepath.Join(logRoot, "workass-headless.out.log"))
	writeXMLKeyValue(&b, "StandardErrorPath", filepath.Join(logRoot, "workass-headless.err.log"))
	b.WriteString("<key>EnvironmentVariables</key><dict>")
	for _, key := range []string{"HOME", "PATH", "WORKASS_PROFILE", "WORKASS_DATA_ROOT"} {
		if value := env[key]; value != "" {
			writeXMLKeyValue(&b, key, value)
		}
	}
	b.WriteString("</dict></dict></plist>\n")
	return b.String()
}

func writeXMLKeyValue(b *strings.Builder, key, value string) {
	b.WriteString("<key>")
	b.WriteString(xmlEscapeString(key))
	b.WriteString("</key><string>")
	b.WriteString(xmlEscapeString(value))
	b.WriteString("</string>")
}

func xmlEscapeString(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}
