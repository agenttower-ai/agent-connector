package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func cursorStatus(event string, input map[string]any) string {
	switch event {
	case "sessionStart", "afterAgentResponse":
		return "idle"
	case "beforeSubmitPrompt":
		return "thinking"
	case "preToolUse":
		return "busy"
	case "postToolUse":
		return "thinking"
	case "postToolUseFailure":
		return "error"
	case "stop":
		if str(input["status"]) == "error" {
			return "error"
		}
		return "idle"
	case "sessionEnd":
		if str(input["reason"]) == "error" {
			return "error"
		}
		return "idle"
	case "beforeShellExecution", "beforeMCPExecution":
		return "alarm"
	}
	return ""
}

func cursorHook() error {
	data, e := io.ReadAll(io.LimitReader(os.Stdin, 2<<20))
	if e != nil {
		return e
	}
	input, e := decodeCursorInput(data)
	if e != nil {
		return e
	}
	event := str(input["hook_event_name"])
	if event == "workspaceOpen" {
		return cursorWorkspaceOpen(input)
	}
	permission := event == "beforeShellExecution" || event == "beforeMCPExecution"
	session := str(input["conversation_id"])
	if session == "" {
		session = str(input["session_id"])
	}
	if session == "" || cursorStatus(event, input) == "" {
		if permission {
			cursorDecision(false)
		}
		return nil
	}
	raw, e := cursorLocalURL()
	if e != nil {
		if permission {
			cursorDecision(false)
		}
		return e
	}
	conn, _, e := websocket.DefaultDialer.Dial(raw, nil)
	if e != nil {
		if permission {
			cursorDecision(false)
		}
		return nil
	}
	defer conn.Close()
	deadline := 2 * time.Second
	if permission {
		deadline = 110 * time.Second
	}
	conn.SetReadDeadline(time.Now().Add(deadline))
	id := "cursor:" + session
	send := func(m map[string]any) error { m["instanceId"] = id; return conn.WriteJSON(m) }
	directory := str(input["cwd"])
	if directory == "" {
		if roots, ok := input["workspace_roots"].([]any); ok && len(roots) > 0 {
			directory = str(roots[0])
		}
	}
	directory = cursorRepoDirectory(directory)
	host, _ := os.Hostname()
	kind := "event"
	if permission {
		kind = "control"
	}
	// Only the Desktop Bridge can submit to a running desktop conversation.
	// Hooks also run in cursor-agent, whose interactive session has no such channel.
	if e = send(map[string]any{"type": "monitor.hello", "connection": kind, "instance": map[string]any{"id": id, "agent": "cursor", "hostname": host, "directory": directory, "contextId": session, "capabilities": map[string]any{"sessionControl": false, "history": true, "permission": permission}}}); e != nil {
		if permission {
			cursorDecision(false)
		}
		return nil
	}
	if e = send(map[string]any{"type": "monitor.session", "session": map[string]any{"id": session, "agent": "cursor", "contextId": session, "directory": directory, "title": directory, "status": cursorStatus(event, input), "updatedAt": time.Now().UnixMilli()}}); e != nil {
		if permission {
			cursorDecision(false)
		}
		return nil
	}
	if !permission {
		return nil
	}
	requestID := randomID()
	label := "Shell"
	patterns := []string{str(input["command"])}
	if event == "beforeMCPExecution" {
		label = "MCP: " + str(input["tool_name"])
		params, _ := json.Marshal(input["tool_input"])
		patterns = []string{string(params)}
	}
	if e = send(map[string]any{"type": "monitor.permission", "request": map[string]any{"id": requestID, "sessionID": session, "permission": label, "patterns": patterns, "createdAt": time.Now().UnixMilli()}}); e != nil {
		cursorDecision(false)
		return nil
	}
	allow := false
	for {
		var m map[string]any
		if e = conn.ReadJSON(&m); e != nil {
			break
		}
		if str(m["type"]) == "control.permission.reply" && str(m["requestID"]) == requestID {
			allow = str(m["reply"]) == "once"
			break
		}
	}
	reply := "reject"
	if allow {
		reply = "once"
	}
	_ = send(map[string]any{"type": "monitor.permission.resolved", "requestID": requestID, "reply": reply})
	cursorDecision(allow)
	return nil
}

func decodeCursorInput(data []byte) (map[string]any, error) {
	input := map[string]any{}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if e := json.Unmarshal(data, &input); e != nil {
		return nil, e
	}
	return input, nil
}

func cursorLocalURL() (string, error) {
	raw := os.Getenv("AGENT_LIGHT_URL")
	if raw == "" {
		raw = "ws://127.0.0.1:17654"
	}
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "ws" && u.Scheme != "wss") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return "", errors.New("Cursor hook 仅连接本机 Connector")
	}
	return raw, nil
}

func cursorWorkspaceOpen(input map[string]any) error {
	roots, _ := input["workspace_roots"].([]any)
	if len(roots) == 0 {
		return nil
	}
	raw, e := cursorLocalURL()
	if e != nil {
		return e
	}
	conn, _, e := websocket.DefaultDialer.Dial(raw, nil)
	if e != nil {
		return nil
	}
	defer conn.Close()
	host, _ := os.Hostname()
	seen := map[string]bool{}
	for _, root := range roots {
		directory := strings.TrimSpace(str(root))
		if directory == "" {
			continue
		}
		directory = cursorRepoDirectory(directory)
		key := directory
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		hash := sha256.Sum256([]byte(key))
		id := "cursor:workspace:" + hex.EncodeToString(hash[:8])
		if e = conn.WriteJSON(map[string]any{"type": "monitor.hello", "connection": "event", "instanceId": id, "instance": map[string]any{"id": id, "agent": "cursor", "hostname": host, "directory": directory, "contextId": directory, "capabilities": map[string]any{"sessionControl": false, "permission": false}}}); e != nil {
			return e
		}
	}
	return nil
}

func cursorRepoDirectory(directory string) string {
	if directory == "" {
		return ""
	}
	for root := filepath.Clean(directory); ; root = filepath.Dir(root) {
		gitPath := filepath.Join(root, ".git")
		if info, e := os.Stat(gitPath); e == nil {
			if info.IsDir() {
				return root
			}
			if data, e := os.ReadFile(gitPath); e == nil {
				line := strings.TrimSpace(string(data))
				if strings.HasPrefix(line, "gitdir: ") {
					gitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir: "))
					if !filepath.IsAbs(gitDir) {
						gitDir = filepath.Join(root, gitDir)
					}
					if common, e := os.ReadFile(filepath.Join(gitDir, "commondir")); e == nil {
						commonDir := filepath.Clean(filepath.Join(gitDir, strings.TrimSpace(string(common))))
						if filepath.Base(commonDir) == ".git" {
							return filepath.Dir(commonDir)
						}
					}
				}
			}
			return root
		}
		if parent := filepath.Dir(root); parent == root {
			break
		}
	}
	return directory
}

func cursorDecision(allow bool) {
	decision := "deny"
	if allow {
		decision = "allow"
	}
	b, _ := json.Marshal(map[string]any{"permission": decision})
	fmt.Print(string(b))
}
