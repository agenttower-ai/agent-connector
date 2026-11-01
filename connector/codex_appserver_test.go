package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexHistoryReadsOnlyConversationMessages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	id := "01a0d35c-4c1c-7482-9aeb-93df8282adbe"
	dir := filepath.Join(home, "sessions", "2026", "09", "28")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data := strings.Join([]string{
		`{"timestamp":"2026-09-28T00:00:00Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"private"}]}}`,
		`{"timestamp":"2026-09-28T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
		`{"timestamp":"2026-09-28T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-09-28T00-00-00-"+id+".jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	messages, err := codexHistory(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || obj(messages[0])["text"] != "hello" || obj(messages[1])["text"] != "world" {
		t.Fatalf("unexpected messages: %#v", messages)
	}
	if _, err := codexHistory("../other"); err == nil {
		t.Fatal("accepted unsafe thread ID")
	}
}

func TestCodexThreadRefreshPreservesProjectCards(t *testing.T) {
	s := newState()
	s.replaceCodexDesktop(map[string]map[string]any{"codex-desktop:project": {"id": "codex-desktop:project"}}, map[string]map[string]any{})
	s.replaceCodexThreads(map[string]map[string]any{"codex-thread:old": {"id": "codex-thread:old"}}, map[string]map[string]any{recordKey("codex-thread:old", "old"): {"id": "old", "instanceId": "codex-thread:old"}})
	s.replaceCodexThreads(map[string]map[string]any{}, map[string]map[string]any{})
	snap := s.snapshot()
	if len(snap["instances"].([]any)) != 1 || len(snap["sessions"].([]any)) != 0 {
		t.Fatalf("stale Codex thread remains: %#v", snap)
	}
}

func TestCodexRolloutFileRejectsInvalidID(t *testing.T) {
	if _, err := codexRolloutFile("not-a-thread"); err == nil {
		t.Fatal("accepted invalid thread")
	}
}

func TestCodexExecutablePrefersUpdatedDesktopCLI(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("AGENTTOWER_CODEX_CLI", "")
	bin := filepath.Join(local, "OpenAI", "Codex", "bin")
	old := filepath.Join(bin, "codex.exe")
	previous := filepath.Join(bin, "previous-release", "codex.exe")
	latest := filepath.Join(bin, "new-release", "codex.exe")
	for _, path := range []string{old, previous, latest} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("cli"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(previous, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := codexExecutable(); got != latest {
		t.Fatalf("selected stale CLI %q instead of %q", got, latest)
	}
	if err := os.Remove(latest); err != nil {
		t.Fatal(err)
	}
	if got := codexExecutable(); got != previous {
		t.Fatalf("versioned CLI fallback: %q", got)
	}
	if err := os.Remove(previous); err != nil {
		t.Fatal(err)
	}
	if got := codexExecutable(); got != old {
		t.Fatalf("legacy CLI fallback: %q", got)
	}
	t.Setenv("AGENTTOWER_CODEX_CLI", "custom-codex")
	if got := codexExecutable(); got != "custom-codex" {
		t.Fatalf("explicit override ignored: %q", got)
	}
}

func TestCodexDesktopThreadDiscoveryUsesRealAssignments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	id := "01a0d35c-4c1c-7482-9aeb-93df8282adbe"
	project := codexDesktopProject{ID: "project", Name: "Demo", RootPaths: []string{home}}
	state := codexDesktopState{LocalProjects: map[string]codexDesktopProject{"project": project}}
	state.ThreadProjects = map[string]struct {
		ProjectID string `json:"projectId"`
	}{id: {ProjectID: "project"}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex-global-state.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sessions", "2026", "09", "28")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-09-28T00-00-00-"+id+".jsonl"), []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(`{"id":"`+id+`","thread_name":"Existing chat","updated_at":"2026-09-28T00:00:00Z"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &connector{state: newState(), codexRunning: map[string]bool{}}
	c.syncCodexThreads([]codexDesktopProject{project})
	snap := c.state.snapshot()
	if len(snap["instances"].([]any)) != 1 || len(snap["sessions"].([]any)) != 1 {
		t.Fatalf("unexpected discovery: %#v", snap)
	}
	session := obj(snap["sessions"].([]any)[0])
	if session["id"] != id || session["title"] != "Existing chat" {
		t.Fatalf("unexpected session: %#v", session)
	}
	instance := obj(snap["instances"].([]any)[0])
	if obj(instance["capabilities"])["sessionControl"] != true {
		t.Fatal("real thread cannot be resumed")
	}
}
