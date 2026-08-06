//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

func windowsTaskCommand(options headlessServiceOptions) string {
	return fmt.Sprintf("%s --prod --headless --state-dir %s --port %s --bind %s",
		quoteWindowsArg(options.Executable),
		quoteWindowsArg(filepath.Clean(options.StateDir)),
		strconv.Itoa(options.Port),
		quoteWindowsArg(options.Bind),
	)
}

func quoteWindowsArg(value string) string {
	return `"` + value + `"`
}

func installHeadlessService(options headlessServiceOptions) error {
	if options.Executable == "" {
		return fmt.Errorf("the executable path is unavailable")
	}
	if options.Port < 1 || options.Port > 65535 {
		return fmt.Errorf("invalid service port %d", options.Port)
	}
	if options.Bind != "localhost" && options.Bind != "lan" {
		return fmt.Errorf("invalid service bind %q", options.Bind)
	}
	if err := os.MkdirAll(options.StateDir, 0o700); err != nil {
		return fmt.Errorf("create service state directory: %w", err)
	}
	xmlPath := filepath.Join(options.StateDir, ".workass-headless-task.xml")
	if err := os.WriteFile(xmlPath, utf16LEXML(windowsTaskXML(options)), 0o600); err != nil {
		return fmt.Errorf("write scheduled task definition: %w", err)
	}
	defer os.Remove(xmlPath)
	args := []string{"/Create", "/TN", serviceName(), "/XML", xmlPath, "/F"}
	if output, err := exec.Command("schtasks.exe", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks create: %w (%s)", err, trimServiceOutput(output))
	}
	if output, err := exec.Command("schtasks.exe", "/Run", "/TN", serviceName()).CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks start: %w (%s)", err, trimServiceOutput(output))
	}
	return nil
}

func windowsTaskXML(options headlessServiceOptions) string {
	args := fmt.Sprintf("--prod --headless --state-dir %s --port %s --bind %s",
		quoteWindowsArg(filepath.Clean(options.StateDir)), strconv.Itoa(options.Port), quoteWindowsArg(options.Bind))
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Workass headless daemon</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
  </Settings>
  <Actions Context="Author"><Exec><Command>%s</Command><Arguments>%s</Arguments><WorkingDirectory>%s</WorkingDirectory></Exec></Actions>
</Task>
`, xmlEscape(options.Executable), xmlEscape(args), xmlEscape(filepath.Dir(options.Executable)))
}

func utf16LEXML(value string) []byte {
	units := utf16.Encode([]rune(value))
	output := make([]byte, 2+len(units)*2)
	output[0], output[1] = 0xff, 0xfe
	for index, unit := range units {
		binary.LittleEndian.PutUint16(output[2+index*2:], unit)
	}
	return output
}

func xmlEscape(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
