package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func codebuddyStatus(event string) string {
	switch event {
	case "SessionStart", "Stop", "SessionEnd":
		return "idle"
	case "UserPromptSubmit", "PostToolUse":
		return "thinking"
	case "PreToolUse":
		return "busy"
	case "PostToolUseFailure":
		return "error"
	}
	return ""
}

func codebuddyDecision(allow bool) {
	decision := "deny"
	if allow {
		decision = "allow"
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": decision, "permissionDecisionReason": "AgentTower 手机审批被拒绝或连接超时"}})
	fmt.Print(string(b))
}

func codebuddyHook() error {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 2<<20))
	if err != nil {
		return err
	}
	input := map[string]any{}
	if err = json.Unmarshal(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}), &input); err != nil {
		return err
	}
	event := str(input["hook_event_name"])
	tool := str(input["tool_name"])
	permission := event == "PreToolUse" && (tool == "Bash" || strings.HasPrefix(tool, "mcp__"))
	session := str(input["session_id"])
	if session == "" || codebuddyStatus(event) == "" {
		if permission {
			codebuddyDecision(false)
		}
		return nil
	}
	raw, err := cursorLocalURL()
	if err != nil {
		if permission {
			codebuddyDecision(false)
		}
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.Dial(raw, nil)
	if err != nil {
		if permission {
			codebuddyDecision(false)
		}
		return nil
	}
	defer conn.Close()
	deadline := 2 * time.Second
	if permission {
		deadline = 110 * time.Second
	}
	_ = conn.SetReadDeadline(time.Now().Add(deadline))
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	id := "codebuddy:" + session
	send := func(m map[string]any) error { m["instanceId"] = id; return conn.WriteJSON(m) }
	directory := str(input["cwd"])
	if directory == "" {
		directory, _ = os.Getwd()
	}
	host, _ := os.Hostname()
	kind := "event"
	if permission {
		kind = "control"
	}
	instance := map[string]any{"id": id, "agent": "codebuddy", "hostname": host, "directory": directory, "contextId": session, "capabilities": map[string]any{"sessionControl": false, "permission": true}}
	if err = send(map[string]any{"type": "monitor.hello", "connection": kind, "instance": instance}); err == nil {
		status := codebuddyStatus(event)
		if permission {
			status = "alarm"
		}
		err = send(map[string]any{"type": "monitor.session", "session": map[string]any{"id": session, "agent": "codebuddy", "contextId": session, "directory": directory, "title": directory, "status": status, "updatedAt": time.Now().UnixMilli()}})
	}
	if !permission {
		return nil
	}
	if err != nil {
		codebuddyDecision(false)
		return nil
	}
	requestID := randomID()
	params, _ := json.Marshal(input["tool_input"])
	if err = send(map[string]any{"type": "monitor.permission", "request": map[string]any{"id": requestID, "sessionID": session, "permission": tool, "patterns": []string{string(params)}, "createdAt": time.Now().UnixMilli()}}); err != nil {
		codebuddyDecision(false)
		return nil
	}
	allow := false
	for {
		var reply map[string]any
		if err = conn.ReadJSON(&reply); err != nil {
			break
		}
		if str(reply["type"]) == "control.permission.reply" && str(reply["requestID"]) == requestID {
			allow = str(reply["reply"]) == "once"
			break
		}
	}
	reply := "reject"
	if allow {
		reply = "once"
	}
	_ = send(map[string]any{"type": "monitor.permission.resolved", "requestID": requestID, "reply": reply})
	codebuddyDecision(allow)
	return nil
}
