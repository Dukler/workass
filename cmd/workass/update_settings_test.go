package main

import "testing"

// "Instalar solo" (Ajustes · Actualizaciones) lives in the settings blob, which
// settings:set replaces whole: a key normalizeSettings does not carry is lost on
// the next save from any window.
func TestAutoInstallUpdatesSettingIsOffByDefaultAndSurvivesASave(t *testing.T) {
	if got := normalizeSettings(nil)["autoInstallUpdates"]; got != false {
		t.Fatalf("default autoInstallUpdates = %#v, want false", got)
	}
	if got := normalizeSettings(map[string]any{"autoInstallUpdates": true})["autoInstallUpdates"]; got != true {
		t.Fatalf("saved autoInstallUpdates = %#v, want true", got)
	}
	if got := normalizeSettings(map[string]any{"autoInstallUpdates": "yes"})["autoInstallUpdates"]; got != false {
		t.Fatalf("a non-boolean autoInstallUpdates = %#v, want the default", got)
	}
	again := normalizeSettings(normalizeSettings(map[string]any{"autoInstallUpdates": true}))
	if again["autoInstallUpdates"] != true {
		t.Fatalf("a normalized blob lost the setting: %#v", again)
	}
}
