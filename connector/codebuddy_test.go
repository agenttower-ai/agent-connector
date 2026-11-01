package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func TestCodeBuddyStatus(t *testing.T) {
	for event, want := range map[string]string{"SessionStart": "idle", "UserPromptSubmit": "thinking", "PreToolUse": "busy", "PostToolUse": "thinking", "PostToolUseFailure": "error", "Stop": "idle", "SessionEnd": "idle", "unknown": ""} {
		if got := codebuddyStatus(event); got != want {
			t.Errorf("%s: got %q, want %q", event, got, want)
		}
	}
}

func TestCodeBuddyApprovalFailsClosedWhenConnectorUnavailable(t *testing.T) {
	t.Setenv("AGENT_LIGHT_URL", "ws://127.0.0.1:1")
	oldIn, oldOut := os.Stdin, os.Stdout
	in, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err = in.WriteString(`{"hook_event_name":"PreToolUse","session_id":"test-session","tool_name":"Bash","tool_input":{"command":"pwd"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err = in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	os.Stdin, os.Stdout = in, out
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	if err = codebuddyHook(); err != nil {
		t.Fatal(err)
	}
	if _, err = out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	var decision map[string]any
	if err = json.Unmarshal(b, &decision); err != nil {
		t.Fatal(err)
	}
	if str(obj(decision["hookSpecificOutput"])["permissionDecision"]) != "deny" || !strings.Contains(string(b), "PreToolUse") {
		t.Fatalf("approval did not fail closed: %s", b)
	}
}
