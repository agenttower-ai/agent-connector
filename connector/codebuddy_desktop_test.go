package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCodebuddyDesktopHistoryAndInvalidID(t *testing.T) {
	id := "0339789967ee48738d5fbd5c453a0907"
	dir := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(filepath.Join(dir, "messages"), 0700); err != nil {
		t.Fatal(err)
	}
	index, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"id": "first", "role": "user"}, map[string]any{"id": "second", "role": "assistant"}, map[string]any{"id": "../../../bad", "role": "user"}}})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), index, 0600); err != nil {
		t.Fatal(err)
	}
	for id, role := range map[string]string{"first": "user", "second": "assistant"} {
		b, _ := json.Marshal(map[string]string{"role": role, "message": role + " message", "createdAt": "2026-10-01T12:00:00Z"})
		if err := os.WriteFile(filepath.Join(dir, "messages", id+".json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := codebuddyDesktopHistory(dir, id)
	if err != nil || len(rows) != 2 || rows[0]["text"] != "user message" || rows[1]["role"] != "assistant" {
		t.Fatalf("history: %v, %v", rows, err)
	}
	if _, err := codebuddyDesktopHistory(dir, "../bad"); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestCodebuddyMessageText(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"role":"user","content":[{"type":"text","text":"<additional_data>Below are some potentially helpful pieces</additional_data>\n<user_query>AgentTower 连接测试：请只回复 AGENTTOWER_OK，不执行工具、不修改文件。</user_query>"}]}`, "AgentTower 连接测试：请只回复 AGENTTOWER_OK，不执行工具、不修改文件。"},
		{`{"role":"assistant","content":[{"type":"text","text":"**完成**\n请检查结果"}]}`, "**完成**\n请检查结果"},
		{`{"role":"assistant","content":[{"type":"thinking","text":"private"},{"type":"text","text":"第一段"},{"type":"text","text":"第二段"}]}`, "第一段\n\n第二段"},
		{`普通消息`, "普通消息"},
		{`{"role":"assistant","content":[{"type":"tool_use","text":"private"}]}`, ""},
	}
	for _, tc := range cases {
		if got := codebuddyMessageText(tc.raw); got != tc.want {
			t.Errorf("codebuddyMessageText(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestCodebuddyDesktopHistoryUnwrapsChatEnvelope(t *testing.T) {
	id := "0339789967ee48738d5fbd5c453a0907"
	dir := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(filepath.Join(dir, "messages"), 0700); err != nil {
		t.Fatal(err)
	}
	index, _ := json.Marshal(map[string]any{"messages": []any{map[string]string{"id": "first", "role": "user"}, map[string]string{"id": "second", "role": "assistant"}}})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), index, 0600); err != nil {
		t.Fatal(err)
	}
	for id, message := range map[string]string{
		"first":  `{"role":"user","content":[{"type":"text","text":"<additional_data>context</additional_data>\n<user_query>你好</user_query>"}]}`,
		"second": `{"role":"assistant","content":[{"type":"text","text":"**你好**"}]}`,
	} {
		role := "user"
		if id == "second" {
			role = "assistant"
		}
		data, _ := json.Marshal(map[string]string{"role": role, "message": message, "createdAt": "2026-10-01T12:00:00Z"})
		if err := os.WriteFile(filepath.Join(dir, "messages", id+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := codebuddyDesktopHistory(dir, id)
	if err != nil || len(rows) != 2 || rows[0]["text"] != "你好" || rows[1]["text"] != "**你好**" {
		t.Fatalf("history: %v, %v", rows, err)
	}
}

func TestCodebuddyDesktopSessionsFromRealLayout(t *testing.T) {
	root := os.Getenv("AGENTTOWER_CODEBUDDY_DATA_DIR")
	if root == "" {
		root = codebuddyDesktopRoot()
	}
	if _, err := os.Stat(root); err != nil {
		t.Skip("CodeBuddy IDE history not installed")
	}
	rows, err := codebuddyDesktopSessions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("CodeBuddy IDE session not discovered")
	}
	for _, row := range rows {
		if row.Directory == "" || row.ID == "" {
			t.Fatalf("incomplete session: %+v", row)
		}
		messages, err := codebuddyDesktopHistory(row.HistoryDir, row.ID)
		if err != nil || len(messages) == 0 {
			t.Fatalf("empty session included: %s", row.ID)
		}
	}
}

func TestCodebuddyBridgeNeverForwarded(t *testing.T) {
	c := &connector{state: newState(), disabledProjects: map[string]projectRef{}}
	c.state.observe(map[string]any{"type": "monitor.hello", "instanceId": "bridge", "instance": map[string]any{"id": "bridge", "agent": "codebuddy-bridge", "directory": "project"}})
	if len(c.forwardedSnapshot()["instances"].([]any)) != 0 {
		t.Fatal("internal bridge exposed to phone")
	}
	if c.shouldForwardEvent(map[string]any{"type": "monitor.heartbeat", "instanceId": "bridge"}) {
		t.Fatal("bridge event forwarded")
	}
}
