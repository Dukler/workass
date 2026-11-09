package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"workass/internal/voice"
	"workass/internal/wire"
)

func TestGeneralSettingsDefaults(t *testing.T) {
	settings := normalizeSettings(nil)
	want := map[string]any{
		"newChat":      map[string]any{"providerId": "", "model": "", "effort": "", "modeId": "", "folder": "last"},
		"whileWorking": "steer",
		"notices":      map[string]any{"needsYou": true, "turnEnded": true, "jobs": true, "sound": false},
		"dictation":    map[string]any{"lang": "es"},
	}
	for key, value := range want {
		if !reflect.DeepEqual(settings[key], value) {
			t.Errorf("default %s = %#v, want %#v", key, settings[key], value)
		}
	}
	if !reflect.DeepEqual(normalizeSettings(map[string]any{"prApprover": "x"})["notices"], want["notices"]) {
		t.Fatalf("a blob without the key keeps the default notices")
	}
}

func TestGeneralSettingsSurviveAWriteAndRejectJunk(t *testing.T) {
	store := newAppSettingsStore(filepath.Join(t.TempDir(), "app-settings.json"))
	saved, err := store.set(map[string]any{
		"newChat":      map[string]any{"providerId": " agent-a ", "model": "large", "effort": "high", "modeId": "edit", "folder": "/tmp/project", "extra": 1},
		"whileWorking": "steer",
		"notices":      map[string]any{"needsYou": false, "turnEnded": "false", "jobs": true, "sound": true, "other": true},
		"dictation":    map[string]any{"lang": "EN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	read := store.get()
	for _, settings := range []map[string]any{saved, read} {
		if got, want := settings["newChat"], map[string]any{"providerId": "agent-a", "model": "large", "effort": "high", "modeId": "edit", "folder": "/tmp/project"}; !reflect.DeepEqual(got, want) {
			t.Errorf("newChat = %#v, want %#v", got, want)
		}
		if settings["whileWorking"] != "steer" {
			t.Errorf("whileWorking = %#v", settings["whileWorking"])
		}
		if got, want := settings["notices"], map[string]any{"needsYou": false, "turnEnded": false, "jobs": true, "sound": true}; !reflect.DeepEqual(got, want) {
			t.Errorf("notices = %#v, want %#v", got, want)
		}
		if dictationLanguage(settings) != "en" {
			t.Errorf("dictation = %#v", settings["dictation"])
		}
	}

	junk := normalizeSettings(map[string]any{
		"newChat":      map[string]any{"model": "large", "effort": "high", "modeId": "edit", "folder": "relative/path"},
		"whileWorking": "interrupt",
		"notices":      "all",
		"dictation":    map[string]any{"lang": "español"},
	})
	if got, want := junk["newChat"], map[string]any{"providerId": "", "model": "", "effort": "", "modeId": "", "folder": "last"}; !reflect.DeepEqual(got, want) {
		t.Errorf("without an agent nothing else is kept: %#v", got)
	}
	if junk["whileWorking"] != "steer" || dictationLanguage(junk) != "es" {
		t.Errorf("junk whileWorking=%#v dictation=%#v", junk["whileWorking"], junk["dictation"])
	}
	effortless := normalizeNewChatSettings(map[string]any{"providerId": "agent-a", "effort": "high", "folder": "active"})
	if effortless["effort"] != "" || effortless["folder"] != "active" {
		t.Errorf("an effort needs its model: %#v", effortless)
	}
	if got := normalizeNewChatFolder(`C:\work\repo`); got != `C:\work\repo` {
		t.Errorf("windows folder = %q", got)
	}
	if got := dictationLanguage(normalizeSettings(map[string]any{"dictation": map[string]any{"lang": "auto"}})); got != "auto" {
		t.Errorf("auto = %q", got)
	}
}

// voice:transcribe without a language tells whisper the one in settings, and
// a language the caller names still wins.
func TestVoiceTranscribeUsesTheDictationLanguage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake whisper is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-whisper")
	script := "#!/bin/sh\nlang=none\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"-l\" ]; then lang=\"$2\"; fi\n  shift\ndone\necho \"lang $lang\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKASS_WHISPER_BIN", bin)
	t.Setenv("WORKASS_WHISPER_MODEL", filepath.Join(dir, "model.bin"))

	settings := map[string]any{"dictation": map[string]any{"lang": "auto"}}
	hub := wire.NewHub()
	registerVoiceHandlers(hub, func() map[string]any { return normalizeSettings(settings) })
	audio := base64.StdEncoding.EncodeToString(voice.WAV(make([]byte, 320)))
	transcribe := func(args map[string]any) string {
		t.Helper()
		result, err := hub.Invoke("voice:transcribe", []any{args})
		if err != nil {
			t.Fatal(err)
		}
		return fieldString(mapFromAnyMain(result), "text")
	}
	if got := transcribe(map[string]any{"audio": audio}); got != "lang auto" {
		t.Fatalf("setting auto: %q", got)
	}
	settings = map[string]any{}
	if got := transcribe(map[string]any{"audio": audio}); got != "lang es" {
		t.Fatalf("default: %q", got)
	}
	if got := transcribe(map[string]any{"audio": audio, "lang": "en"}); got != "lang en" {
		t.Fatalf("caller's language: %q", got)
	}
}
