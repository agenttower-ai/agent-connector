package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func install(agent, target string) error {
	switch agent {
	case "codebuddy-cli":
		agent = "codebuddy"
	case "cursor-cli":
		agent = "cursor"
	case "opencode-cli":
		agent = "opencode"
	case "codex-cli":
		agent = "codex"
	}
	home, _ := os.UserHomeDir()
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	exe, e = filepath.Abs(exe)
	if e != nil {
		return e
	}
	switch agent {
	case "codebuddy-desktop":
		if target == "" {
			target = filepath.Join(home, ".codebuddycn", "extensions", "agenttower.codebuddy-desktop-0.1.0")
		}
		if e = os.MkdirAll(target, 0700); e != nil {
			return e
		}
		for name, dest := range map[string]string{"codebuddy-desktop.js": "extension.js", "codebuddy-desktop.json": "package.json"} {
			data, readErr := assets.ReadFile("adapters/" + name)
			if readErr != nil {
				return readErr
			}
			path := filepath.Join(target, dest)
			if existing, readErr := os.ReadFile(path); readErr == nil && !bytes.Equal(existing, data) && !bytes.Contains(existing, []byte("AgentTower")) {
				return errors.New("目标文件属于其他扩展，拒绝覆盖")
			}
			if e = os.WriteFile(path, data, 0600); e != nil {
				return e
			}
		}
		fmt.Printf("CodeBuddy CN 桌面桥已安装到 %s，请重启 CodeBuddy CN。\n", target)
		return nil
	case "codebuddy":
		if target == "" {
			target = filepath.Join(home, ".codebuddy", "settings.json")
		}
		settings := map[string]any{}
		if b, readErr := os.ReadFile(target); readErr == nil {
			if e = json.Unmarshal(b, &settings); e != nil {
				return fmt.Errorf("CodeBuddy settings.json 无效：%w", e)
			}
			if settings == nil {
				return errors.New("CodeBuddy settings.json 必须是 JSON 对象")
			}
			backup := fmt.Sprintf("%s.agenttower-%d.bak", target, time.Now().UnixMilli())
			if e = os.WriteFile(backup, b, 0600); e != nil {
				return e
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		hooks := obj(settings["hooks"])
		if hooks == nil {
			if settings["hooks"] != nil {
				return errors.New("CodeBuddy hooks 配置必须是 JSON 对象")
			}
			hooks = map[string]any{}
			settings["hooks"] = hooks
		}
		command := fmt.Sprintf("\"%s\" codebuddy-hook", strings.ReplaceAll(filepath.ToSlash(exe), "\"", "\\\""))
		for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop", "SessionEnd"} {
			groups, ok := hooks[event].([]any)
			if hooks[event] != nil && !ok {
				return fmt.Errorf("CodeBuddy hooks.%s 必须是数组", event)
			}
			filtered := make([]any, 0, len(groups)+1)
			for _, group := range groups {
				g := obj(group)
				items, _ := g["hooks"].([]any)
				keep := make([]any, 0, len(items))
				for _, item := range items {
					if !strings.HasSuffix(str(obj(item)["command"]), " codebuddy-hook") {
						keep = append(keep, item)
					}
				}
				if len(keep) > 0 || g["hooks"] == nil {
					n := clone(g)
					if len(keep) > 0 {
						n["hooks"] = keep
					}
					filtered = append(filtered, n)
				}
			}
			group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 5}}}
			if event == "PreToolUse" {
				group["matcher"] = "^(Bash|mcp__.*)$"
				group["hooks"] = []any{map[string]any{"type": "command", "command": command, "timeout": 120}}
			}
			filtered = append(filtered, group)
			hooks[event] = filtered
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		b, e := json.MarshalIndent(settings, "", "  ")
		if e != nil {
			return e
		}
		if e = os.WriteFile(target, b, 0600); e != nil {
			return e
		}
		fmt.Printf("CodeBuddy hooks 已合并到 %s，请在 CodeBuddy /hooks 中确认并重启 CodeBuddy Code。\n", target)
		return nil
	case "cursor":
		if target == "" {
			target = filepath.Join(home, ".cursor", "hooks.json")
		}
		settings := map[string]any{}
		if b, readErr := os.ReadFile(target); readErr == nil {
			if e = json.Unmarshal(b, &settings); e != nil {
				return fmt.Errorf("Cursor hooks.json 无效：%w", e)
			}
			backup := fmt.Sprintf("%s.agenttower-%d.bak", target, time.Now().UnixMilli())
			if e = os.WriteFile(backup, b, 0600); e != nil {
				return e
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		if settings["version"] == nil {
			settings["version"] = 1
		}
		hooks := obj(settings["hooks"])
		if hooks == nil {
			hooks = map[string]any{}
			settings["hooks"] = hooks
		}
		command := fmt.Sprintf("\"%s\" cursor-hook", exe)
		statusEvents := []string{"workspaceOpen", "sessionStart", "beforeSubmitPrompt", "preToolUse", "postToolUse", "postToolUseFailure", "afterAgentResponse", "stop", "sessionEnd"}
		approvalEvents := map[string]bool{"beforeShellExecution": true, "beforeMCPExecution": true}
		for _, event := range append(statusEvents, "beforeShellExecution", "beforeMCPExecution") {
			items, _ := hooks[event].([]any)
			filtered := make([]any, 0, len(items)+1)
			for _, item := range items {
				entry := obj(item)
				if str(entry["command"]) != command && !strings.Contains(str(entry["command"]), " cursor-hook") {
					filtered = append(filtered, item)
				}
			}
			timeout := 5
			if approvalEvents[event] {
				timeout = 120
			}
			entry := map[string]any{"command": command, "timeout": timeout}
			if approvalEvents[event] {
				entry["failClosed"] = true
			}
			filtered = append(filtered, entry)
			if len(filtered) > 0 {
				hooks[event] = filtered
			} else {
				delete(hooks, event)
			}
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		b, e := json.MarshalIndent(settings, "", "  ")
		if e != nil {
			return e
		}
		if e = os.WriteFile(target, b, 0600); e != nil {
			return e
		}
		fmt.Printf("Cursor hooks（含 Shell/MCP 审批）已合并到 %s，请重启 Cursor 或 cursor-agent。\n", target)
		return nil
	case "codex":
		if target == "" {
			dir := os.Getenv("CODEX_HOME")
			if dir == "" {
				dir = filepath.Join(home, ".codex")
			}
			target = filepath.Join(dir, "config.toml")
		}
		var existing []byte
		if existing, e = os.ReadFile(target); e != nil && !os.IsNotExist(e) {
			return e
		}
		// Codex accepts one top-level notify command. Never silently replace a
		// notification command owned by another integration.
		lines := strings.Split(strings.TrimPrefix(string(existing), "\ufeff"), "\n")
		kept := make([]string, 0, len(lines))
		inRoot := true
		notifyLine := regexp.MustCompile(`^\s*notify\s*=`)
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "[") {
				inRoot = false
			}
			if inRoot && notifyLine.MatchString(line) {
				if !strings.Contains(line, "# AgentTower Codex notify") || !strings.Contains(line, "\"codex-hook\"") {
					return errors.New("Codex 已配置其他 notify 命令，请先手动处理后再安装")
				}
				continue
			}
			kept = append(kept, line)
		}
		exeJSON, _ := json.Marshal(exe)
		content := "notify = [" + string(exeJSON) + ", \"codex-hook\"] # AgentTower Codex notify\n"
		if len(existing) > 0 {
			content += strings.Join(kept, "\n")
		}
		if len(existing) > 0 {
			backup := fmt.Sprintf("%s.agenttower-%d.bak", target, time.Now().UnixMilli())
			if e = os.WriteFile(backup, existing, 0600); e != nil {
				return e
			}
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		if e = os.WriteFile(target, []byte(content), 0600); e != nil {
			return e
		}
		fmt.Printf("Codex notify 已安装到 %s，请重启 Codex。\n", target)
		return nil
	case "dsh":
		if target == "" {
			target = filepath.Join(home, ".agenttower", "deepseek-harness.mjs")
		}
		target, e = filepath.Abs(target)
		if e != nil {
			return e
		}
		if b, e := os.ReadFile(target); e == nil && !strings.Contains(string(b), "AGENTTOWER-DEEPSEEK-HARNESS-PLUGIN") {
			return errors.New("目标文件属于其他插件，拒绝覆盖")
		}
		b, e := assets.ReadFile("adapters/deepseek-harness.mjs")
		if e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		if e = os.WriteFile(target, b, 0600); e != nil {
			return e
		}
		// A patch overlay keeps Harness's own profile and plugin order intact.
		pluginPath := strings.ReplaceAll(filepath.ToSlash(target), "'", "''")
		patch := filepath.Join(filepath.Dir(target), "deepseek-harness.patch.yml")
		if e = os.WriteFile(patch, []byte("- insert:\n    - id: agenttower-deepseek-harness\n      name: '"+pluginPath+"'\n"), 0600); e != nil {
			return e
		}
		fmt.Printf("DeepSeek Harness 适配器已安装：%s\n启动 Harness 时加入 --patch %s，然后重启 Harness。\n", target, patch)
		return nil
	case "opencode":
		if target == "" {
			dir := os.Getenv("OPENCODE_CONFIG_DIR")
			if dir == "" {
				dir = filepath.Join(home, ".config", "opencode")
			}
			target = filepath.Join(dir, "plugins", "agent-light.js")
		}
		if b, e := os.ReadFile(target); e == nil && !strings.Contains(string(b), "AGENT-TRAFFIC-LIGHT-OPENCODE-PLUGIN") {
			return errors.New("目标文件属于其他插件，拒绝覆盖")
		}
		b, e := assets.ReadFile("adapters/opencode.mjs")
		if e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		if e = os.WriteFile(target, b, 0600); e != nil {
			return e
		}
		fmt.Printf("已安装 %s，请重启 OpenCode。\n", target)
		return nil
	case "claude":
		if target == "" {
			target = filepath.Join(home, ".claude", "settings.json")
		}
		settings := map[string]any{}
		if b, e := os.ReadFile(target); e == nil {
			if e = json.Unmarshal(b, &settings); e != nil {
				return fmt.Errorf("Claude settings.json 无效：%w", e)
			}
			backup := fmt.Sprintf("%s.agenttower-%d.bak", target, time.Now().UnixMilli())
			if e = os.WriteFile(backup, b, 0600); e != nil {
				return e
			}
		}
		hooks := obj(settings["hooks"])
		if hooks == nil {
			hooks = map[string]any{}
			settings["hooks"] = hooks
		}
		command := fmt.Sprintf("\"%s\" claude-hook", exe)
		for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop", "SessionEnd", "PermissionRequest"} {
			groups, _ := hooks[event].([]any)
			filtered := []any{}
			found := false
			for _, g := range groups {
				group := obj(g)
				items, _ := group["hooks"].([]any)
				keep := []any{}
				for _, h := range items {
					item := obj(h)
					old := str(item["command"])
					if old == command {
						found = true
						keep = append(keep, h)
					} else if !strings.Contains(strings.ToLower(old), "claude-hook.cjs") {
						keep = append(keep, h)
					}
				}
				if len(keep) > 0 {
					n := clone(group)
					n["hooks"] = keep
					filtered = append(filtered, n)
				}
			}
			if !found {
				timeout := 5
				if event == "PermissionRequest" {
					timeout = 120
				}
				filtered = append(filtered, map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": command, "timeout": timeout}}})
			}
			hooks[event] = filtered
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		b, e := json.MarshalIndent(settings, "", "  ")
		if e != nil {
			return e
		}
		if e = os.WriteFile(target, b, 0600); e != nil {
			return e
		}
		fmt.Printf("Claude hooks 已合并到 %s，请重启 Claude Code。\n", target)
		return nil
	default:
		return errors.New("支持 opencode、opencode-cli、claude、codebuddy、codebuddy-cli、codebuddy-desktop、cursor、cursor-cli、codex、codex-cli、dsh")
	}
}

func codexHook() error {
	if len(os.Args) < 2 {
		return nil
	}
	input := map[string]any{}
	if e := json.Unmarshal([]byte(os.Args[len(os.Args)-1]), &input); e != nil {
		return e
	}
	if str(input["type"]) != "agent-turn-complete" {
		return nil
	}
	session := str(input["thread-id"])
	if session == "" {
		return nil
	}
	raw := os.Getenv("AGENT_LIGHT_URL")
	if raw == "" {
		raw = "ws://127.0.0.1:17654"
	}
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "ws" && u.Scheme != "wss") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return errors.New("Codex hook 仅连接本机 Connector")
	}
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, e := dialer.Dial(raw, nil)
	if e != nil {
		return nil // A stopped Connector must not interrupt Codex.
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	id := "codex:" + session
	host, _ := os.Hostname()
	directory := str(input["cwd"])
	if directory == "" {
		directory, _ = os.Getwd()
	}
	instance := map[string]any{"id": id, "agent": "codex", "hostname": host, "directory": directory, "contextId": session, "capabilities": map[string]any{"sessionControl": false, "permission": false}}
	if e = conn.WriteJSON(map[string]any{"type": "monitor.hello", "connection": "event", "instanceId": id, "instance": instance}); e != nil {
		return e
	}
	return conn.WriteJSON(map[string]any{"type": "monitor.session", "instanceId": id, "session": map[string]any{"id": session, "agent": "codex", "contextId": session, "directory": directory, "title": directory, "status": "idle", "updatedAt": time.Now().UnixMilli()}})
}

func claudeHook() error {
	data, e := io.ReadAll(io.LimitReader(os.Stdin, 2<<20))
	if e != nil {
		return e
	}
	input := map[string]any{}
	if e = json.Unmarshal(data, &input); e != nil {
		return e
	}
	session := str(input["session_id"])
	if session == "" {
		return nil
	}
	event := str(input["hook_event_name"])
	permission := event == "PermissionRequest"
	id := "claude:" + session
	requestID := randomID()
	raw := os.Getenv("AGENT_LIGHT_URL")
	if raw == "" {
		raw = "ws://127.0.0.1:17654"
	}
	u, e := url.Parse(raw)
	if e != nil {
		return e
	}
	if u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		return errors.New("Claude hook 仅连接本机 Connector")
	}
	conn, _, e := websocket.DefaultDialer.Dial(raw, nil)
	if e != nil {
		if permission {
			printDecision(false)
		}
		return nil
	}
	defer conn.Close()
	deadline := 2 * time.Second
	if permission {
		deadline = 110 * time.Second
	}
	conn.SetReadDeadline(time.Now().Add(deadline))
	send := func(m map[string]any) error { m["instanceId"] = id; return conn.WriteJSON(m) }
	kind := "event"
	if permission {
		kind = "control"
	}
	host, _ := os.Hostname()
	send(map[string]any{"type": "monitor.hello", "connection": kind, "instance": map[string]any{"id": id, "agent": "claude", "hostname": host, "directory": input["cwd"], "contextId": session, "capabilities": map[string]any{"sessionControl": false, "permission": true}}})
	status := map[string]string{"SessionStart": "idle", "UserPromptSubmit": "thinking", "PreToolUse": "busy", "PostToolUse": "thinking", "PostToolUseFailure": "error", "Stop": "idle", "SessionEnd": "idle"}[event]
	if status == "" {
		status = "idle"
	}
	if permission {
		status = "alarm"
	}
	send(map[string]any{"type": "monitor.session", "session": map[string]any{"id": session, "agent": "claude", "contextId": session, "directory": input["cwd"], "title": input["cwd"], "status": status, "updatedAt": time.Now().UnixMilli()}})
	if !permission {
		return nil
	}
	patterns, _ := json.Marshal(input["tool_input"])
	send(map[string]any{"type": "monitor.permission", "request": map[string]any{"id": requestID, "sessionID": session, "permission": input["tool_name"], "patterns": []string{string(patterns)}, "createdAt": time.Now().UnixMilli()}})
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
	send(map[string]any{"type": "monitor.permission.resolved", "requestID": requestID, "reply": reply})
	printDecision(allow)
	return nil
}
func printDecision(allow bool) {
	decision := map[string]any{"behavior": "allow"}
	if !allow {
		decision = map[string]any{"behavior": "deny", "message": "AgentTower 审批被拒绝或连接超时"}
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": decision}})
	fmt.Print(string(b))
}
