package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorHistoryReadsTextOnly(t *testing.T) {
	id := "1134c091-ebc0-4582-92f3-1cf3804913ee"
	root := t.TempDir()
	path := filepath.Join(root, "project", "agent-transcripts", id, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"请修改缩略图"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell"},{"type":"text","text":"已经完成修改"}]}}`,
		`{"type":"status","status":"turn_ended"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	messages, err := cursorHistoryIn(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0]["role"] != "user" || messages[1]["text"] != "已经完成修改" {
		t.Fatalf("unexpected messages: %#v", messages)
	}
	if _, err := cursorHistoryIn(root, "../../secret"); err == nil {
		t.Fatal("accepted invalid session ID")
	}
}
