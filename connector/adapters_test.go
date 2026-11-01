package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCodeBuddyDesktopWritesExtension(t *testing.T) {
	target := filepath.Join(t.TempDir(), "agenttower.codebuddy-desktop-0.1.0")
	if err := install("codebuddy-desktop", target); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(target, "package.json"))
	if err != nil || !strings.Contains(string(manifest), "AgentTower") {
		t.Fatalf("desktop bridge manifest missing: %s, %v", manifest, err)
	}
	code, err := os.ReadFile(filepath.Join(target, "extension.js"))
	if err != nil || !strings.Contains(string(code), "chat.sendMessage") {
		t.Fatalf("desktop bridge entry missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "extension.js"), []byte("other extension"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := install("codebuddy-desktop", target); err == nil {
		t.Fatal("expected foreign extension conflict")
	}
}

func TestInstallCodeBuddyPreservesHooksAndReinstalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := `{"language":"Chinese","hooks":{"PreToolUse":[{"matcher":"^Write$","hooks":[{"type":"command","command":"other-tool"}]}]}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := install("codebuddy-cli", path); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err = json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["language"] != "Chinese" || !strings.Contains(string(b), "other-tool") {
		t.Fatalf("existing settings lost: %s", b)
	}
	if strings.Count(string(b), "codebuddy-hook") != 7 || !strings.Contains(string(b), "^(Bash|mcp__.*)$") {
		t.Fatalf("CodeBuddy hooks missing or duplicated: %s", b)
	}
	backups, err := filepath.Glob(path + ".agenttower-*.bak")
	if err != nil || len(backups) == 0 {
		t.Fatalf("existing settings not backed up: %v", err)
	}
}

func TestInstallOpenCodeCLIUsesGlobalPlugin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-light.js")
	if err := install("opencode-cli", path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "export const OpenCodeBridge") {
		t.Fatal("OpenCode CLI plugin not installed")
	}
}

func TestInstallCodexWritesNotify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("model = \"gpt-5\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := install("codex", path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "notify = [") || !strings.Contains(string(data), "codex-hook") {
		t.Fatalf("Codex notify not installed: %s", data)
	}
}

func TestInstallCodexCLIIsAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := install("codex-cli", path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "codex-hook") {
		t.Fatal("Codex CLI alias not installed")
	}
}

func TestInstallCodexPreservesOtherNotify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("notify = [\"other-tool\"]\n[features]\nfoo = true\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := install("codex", path); err == nil {
		t.Fatal("expected existing notify conflict")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(original) {
		t.Fatalf("existing config changed: %s, %v", data, err)
	}
}

func TestInstallCodexUpdatesOwnNotifyWithoutDuplicating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	for i := 0; i < 2; i++ {
		if err := install("codex", path); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "notify = [") != 1 {
		t.Fatalf("duplicate Codex notify: %s", data)
	}
}
