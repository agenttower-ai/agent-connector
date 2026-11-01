package main

import "testing"

func TestPermissionMemoryIsolation(t *testing.T) {
	for _, agent := range []string{"claude", "cursor", "codebuddy", "DeepSeek Harness"} {
		t.Run(agent, func(t *testing.T) {
			s := newState()
			s.instances["runtime"] = map[string]any{"id": "runtime", "agent": agent, "directory": "project"}
			r := map[string]any{"id": "first", "sessionID": "session", "permission": "bash", "patterns": []any{"npm test"}}
			event := map[string]any{"type": "monitor.permission", "instanceId": "runtime", "request": r}
			if s.permissionMemory(event) {
				t.Fatal("unapproved request allowed")
			}
			s.observe(event)
			if !s.prepareRememberedReply("runtime", "first") {
				t.Fatal("grant not prepared")
			}
			if s.permissionMemory(event) {
				t.Fatal("grant activated before receipt")
			}
			s.permissionMemory(map[string]any{"type": "monitor.permission.resolved", "instanceId": "runtime", "requestID": "first", "reply": "once"})
			if !s.permissionMemory(event) {
				t.Fatal("identical request not remembered")
			}
			for _, field := range []string{"sessionID", "permission", "patterns"} {
				changed := clone(r)
				changed[field] = "different"
				if s.permissionMemory(map[string]any{"type": "monitor.permission", "instanceId": "runtime", "request": changed}) {
					t.Fatalf("crossed %s boundary", field)
				}
			}
			s.instances["runtime"]["directory"] = "other"
			if s.permissionMemory(event) {
				t.Fatal("crossed project boundary")
			}
		})
	}
}

func TestPermissionMemoryRejectReceipt(t *testing.T) {
	s := newState()
	s.instances["runtime"] = map[string]any{"id": "runtime", "agent": "claude"}
	r := map[string]any{"id": "p", "sessionID": "session", "permission": "Bash", "patterns": []any{"command"}}
	event := map[string]any{"type": "monitor.permission", "instanceId": "runtime", "request": r}
	s.observe(event)
	s.prepareRememberedReply("runtime", "p")
	s.permissionMemory(map[string]any{"type": "monitor.permission.resolved", "instanceId": "runtime", "requestID": "p", "reply": "reject"})
	if s.permissionMemory(event) {
		t.Fatal("rejected grant remembered")
	}
}
