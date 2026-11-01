package main

import (
	"path/filepath"
	"testing"
)

func TestForwardedSnapshotFiltersWholeProject(t *testing.T) {
	c := &connector{state: newState(), disabledProjects: map[string]projectRef{}}
	c.state.instances["enabled"] = map[string]any{"id": "enabled", "agent": "codex", "directory": "C:/active"}
	c.state.instances["disabled"] = map[string]any{"id": "disabled", "agent": "codex", "directory": "C:/archive"}
	c.state.sessions[recordKey("enabled", "one")] = map[string]any{"id": "one", "instanceId": "enabled"}
	c.state.sessions[recordKey("disabled", "two")] = map[string]any{"id": "two", "instanceId": "disabled"}
	c.state.permissions[recordKey("disabled", "approval")] = map[string]any{"id": "approval", "instanceId": "disabled"}
	c.disabledProjects[projectKey("codex", "C:/archive")] = projectRef{Agent: "codex", Directory: "C:/archive"}

	snapshot := c.forwardedSnapshot()
	if got := len(snapshot["instances"].([]any)); got != 1 {
		t.Fatalf("instances = %d, want 1", got)
	}
	if got := len(snapshot["sessions"].([]any)); got != 1 {
		t.Fatalf("sessions = %d, want 1", got)
	}
	if got := len(snapshot["permissions"].([]any)); got != 0 {
		t.Fatalf("permissions = %d, want 0", got)
	}
}

func TestForwardingPreferencesLoadFromDeviceConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "device.json")
	config := deviceConfig{Version: 1, DeviceID: "pc", Token: "secret", DisabledProjects: []projectRef{{Agent: "opencode", Directory: "C:/old"}}}
	if err := saveConfig(file, config); err != nil {
		t.Fatal(err)
	}
	c := &connector{configPath: file, disabledProjects: map[string]projectRef{}}
	c.loadForwarding()
	if c.forwardingEnabled("opencode", "C:/old") {
		t.Fatal("disabled project was not restored")
	}
	if !c.forwardingEnabled("opencode", "C:/new") {
		t.Fatal("new projects should forward by default")
	}
}

func TestSetForwardingBulkPersistsAllProjects(t *testing.T) {
	file := filepath.Join(t.TempDir(), "device.json")
	if err := saveConfig(file, deviceConfig{Version: 1, DeviceID: "pc", Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	c := &connector{state: newState(), configPath: file, disabledProjects: map[string]projectRef{}}
	c.state.instances["one"] = map[string]any{"id": "one", "agent": "codex", "directory": "C:/one"}
	c.state.instances["two"] = map[string]any{"id": "two", "agent": "opencode", "directory": "C:/two"}
	projects := []projectRef{{Agent: "codex", Directory: "C:/one"}, {Agent: "opencode", Directory: "C:/two"}}
	if err := c.setForwardingBulk(projects, false); err != nil {
		t.Fatal(err)
	}
	config, err := readConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.DisabledProjects) != 2 || c.forwardingEnabled("codex", "C:/one") || c.forwardingEnabled("opencode", "C:/two") {
		t.Fatalf("bulk disable was not persisted: %#v", config.DisabledProjects)
	}
	if err := c.setForwardingBulk(projects, true); err != nil {
		t.Fatal(err)
	}
	config, err = readConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.DisabledProjects) != 0 || !c.forwardingEnabled("codex", "C:/one") || !c.forwardingEnabled("opencode", "C:/two") {
		t.Fatalf("bulk enable was not persisted: %#v", config.DisabledProjects)
	}
}
