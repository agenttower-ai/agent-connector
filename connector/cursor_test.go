package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorStatuses(t *testing.T) {
	cases := map[string]string{
		"sessionStart": "idle", "beforeSubmitPrompt": "thinking", "preToolUse": "busy",
		"postToolUse": "thinking", "postToolUseFailure": "error", "afterAgentResponse": "idle",
		"stop": "idle", "sessionEnd": "idle", "beforeShellExecution": "alarm",
		"beforeMCPExecution": "alarm",
	}
	for event, want := range cases {
		if got := cursorStatus(event, nil); got != want {
			t.Errorf("%s: got %q, want %q", event, got, want)
		}
	}
	if got := cursorStatus("stop", map[string]any{"status": "error"}); got != "error" {
		t.Errorf("failed stop: %q", got)
	}
}

func TestDecodeCursorInputWithUTF8BOM(t *testing.T) {
	data := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"hook_event_name":"sessionStart","session_id":"abc"}`)...)
	input, e := decodeCursorInput(data)
	if e != nil || input["session_id"] != "abc" {
		t.Fatalf("Cursor BOM input not decoded: %v, %v", input, e)
	}
}

func TestCursorRepoDirectoryForWorktree(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	gitDir := filepath.Join(repo, ".git", "worktrees", "topic")
	worktree := filepath.Join(base, "topic")
	if e := os.MkdirAll(gitDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll(worktree, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitDir+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if got := cursorRepoDirectory(worktree); got != repo {
		t.Fatalf("got %q, want repo %q", got, repo)
	}
}

func TestCursorSessionKeepsImportedTitle(t *testing.T) {
	s := newState()
	id := "cursor:conversation"
	s.observe(map[string]any{"type": "monitor.hello", "instanceId": id, "instance": map[string]any{"id": id, "agent": "cursor", "directory": "C:/repo"}})
	s.observe(map[string]any{"type": "monitor.session", "instanceId": id, "session": map[string]any{"id": "conversation", "agent": "cursor", "directory": "C:/repo", "title": "PDF thumbnail button change", "status": "idle"}})
	s.observe(map[string]any{"type": "monitor.session", "instanceId": id, "session": map[string]any{"id": "conversation", "agent": "cursor", "directory": "C:/repo", "title": "C:/repo", "status": "thinking"}})
	row := obj(s.snapshot()["sessions"].([]any)[0])
	if row["title"] != "PDF thumbnail button change" || row["status"] != "thinking" {
		t.Fatalf("Cursor session title lost: %v", row)
	}
}

func TestCursorHookDoesNotRemoveDesktopControl(t *testing.T) {
	s := newState()
	id := "cursor:conversation"
	s.observe(map[string]any{"type": "monitor.hello", "instanceId": id, "instance": map[string]any{"id": id, "agent": "cursor", "capabilities": map[string]any{"sessionControl": true}}})
	hook := map[string]any{"type": "monitor.hello", "instanceId": id, "instance": map[string]any{"id": id, "agent": "cursor", "capabilities": map[string]any{"sessionControl": false, "history": true}}}
	s.observe(hook)
	instance := obj(s.snapshot()["instances"].([]any)[0])
	if obj(instance["capabilities"])["sessionControl"] != true {
		t.Fatalf("desktop control capability lost: %v", instance)
	}
	if obj(obj(hook["instance"])["capabilities"])["sessionControl"] != true {
		t.Fatalf("relayed hook would remove desktop control: %v", hook)
	}
}

func TestInstallCursorCLIUsesCursorHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	if e := install("cursor-cli", path); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(mustReadCursor(t, path)), "cursor-hook") {
		t.Fatal("Cursor CLI hooks not installed")
	}
	var settings map[string]any
	if e := json.Unmarshal(mustReadCursor(t, path), &settings); e != nil {
		t.Fatal(e)
	}
	hooks := obj(settings["hooks"])
	for _, event := range []string{"beforeShellExecution", "beforeMCPExecution"} {
		items, ok := hooks[event].([]any)
		if !ok || len(items) != 1 || obj(items[0])["failClosed"] != true {
			t.Fatalf("%s approval must be installed by default: %v", event, hooks[event])
		}
	}
}

func TestInstallCursorPreservesHooksAndApprovalByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	initial := []byte(`{"version":2,"hooks":{"stop":[{"command":"existing-hook"}],"beforeShellExecution":[{"command":"existing-approval"}]}}`)
	if e := os.WriteFile(path, initial, 0600); e != nil {
		t.Fatal(e)
	}
	read := func() map[string]any {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		settings := map[string]any{}
		if e = json.Unmarshal(b, &settings); e != nil {
			t.Fatal(e)
		}
		if settings["version"] != float64(2) {
			t.Fatalf("Cursor hooks version changed: %v", settings["version"])
		}
		return obj(settings["hooks"])
	}
	if e := install("cursor", path); e != nil {
		t.Fatal(e)
	}
	hooks := read()
	if len(hooks["stop"].([]any)) != 2 || len(hooks["beforeShellExecution"].([]any)) != 2 {
		t.Fatalf("existing hooks lost: %v", hooks)
	}
	if len(hooks["workspaceOpen"].([]any)) != 1 {
		t.Fatal("workspaceOpen hook missing")
	}
	if obj(hooks["beforeShellExecution"].([]any)[1])["failClosed"] != true {
		t.Fatal("approval hook must fail closed")
	}
	if e := install("cursor", path); e != nil {
		t.Fatal(e)
	}
	hooks = read()
	if len(hooks["stop"].([]any)) != 2 || len(hooks["beforeShellExecution"].([]any)) != 2 || len(hooks["beforeMCPExecution"].([]any)) != 1 {
		t.Fatalf("installation not idempotent: %v", hooks)
	}
	if !strings.Contains(string(mustReadCursor(t, path)), "cursor-hook") {
		t.Fatal("Cursor hook not installed")
	}
}

func mustReadCursor(t *testing.T, path string) []byte {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
