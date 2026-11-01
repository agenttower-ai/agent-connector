package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type cursorDesktopThread struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Source        string `json:"source"`
	Status        string `json:"status"`
	LastUpdatedAt int64  `json:"lastUpdatedAt"`
}

func cursorDesktopDiscoveryPresent() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	dir := os.Getenv("CURSOR_DESKTOP_BRIDGE_DIR")
	if dir == "" {
		dir = filepath.Join(home, ".cursor", "desktop-bridge")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	return err == nil && len(files) > 0
}

func cursorDesktopCommand(ctx context.Context, input string, args ...string) ([]byte, error) {
	var cmd *exec.Cmd
	if custom := os.Getenv("AGENTTOWER_CURSOR_CLI"); custom != "" {
		cmd = exec.CommandContext(ctx, custom, args...)
	} else if runtime.GOOS == "windows" {
		root := filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "cursor")
		exe := filepath.Join(root, "Cursor.exe")
		cli := filepath.Join(root, "resources", "app", "out", "cli.js")
		if _, err := os.Stat(exe); err != nil {
			return nil, errors.New("找不到本机 Cursor CLI")
		}
		if _, err := os.Stat(cli); err != nil {
			return nil, errors.New("找不到本机 Cursor CLI")
		}
		cmd = exec.CommandContext(ctx, exe, append([]string{cli}, args...)...)
		cmd.Env = append(os.Environ(), "ELECTRON_RUN_AS_NODE=1", "VSCODE_DEV=")
	} else {
		cmd = exec.CommandContext(ctx, "cursor", args...)
	}
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Cursor Desktop Bridge 调用失败: %w", err)
	}
	return bytes.TrimSpace(out), nil
}

func cursorDesktopList() ([]cursorDesktopThread, error) {
	if !cursorDesktopDiscoveryPresent() {
		return nil, errors.New("Cursor Desktop Bridge 未启动")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := cursorDesktopCommand(ctx, "", "desktop", "ls", "--json")
	if err != nil {
		return nil, err
	}
	var rows []cursorDesktopThread
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, errors.New("Cursor Desktop Bridge 会话列表无效")
	}
	return rows, nil
}

func cursorDesktopSend(sessionID, message string) error {
	if !cursorConversationID.MatchString(sessionID) || strings.TrimSpace(message) == "" || len(message) > 16000 {
		return errors.New("无效的 Cursor 会话或消息")
	}
	if !cursorDesktopDiscoveryPresent() {
		return errors.New("Cursor Desktop Bridge 未启动，请重启 Cursor")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := cursorDesktopCommand(ctx, message, "desktop", "send", sessionID, "--stdin", "--json")
	if err != nil {
		return err
	}
	var result struct {
		Status   string `json:"status"`
		ThreadID string `json:"threadId"`
		Error    string `json:"error"`
	}
	if json.Unmarshal(out, &result) != nil {
		return errors.New("Cursor Desktop Bridge 发送回执无效")
	}
	if (result.Status == "submitted" || result.Status == "queued") && result.ThreadID == sessionID {
		return nil
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return fmt.Errorf("Cursor Desktop Bridge 未接受消息：%s", result.Status)
}

func (c *connector) cursorBridgeLoop() {
	sync := func() {
		threads, err := cursorDesktopList()
		if err != nil {
			return
		}
		host, _ := os.Hostname()
		known := map[string]map[string]any{}
		for _, value := range c.state.snapshot()["instances"].([]any) {
			instance := obj(value)
			known[str(instance["id"])] = instance
		}
		for _, thread := range threads {
			if thread.Source != "local" || !cursorConversationID.MatchString(thread.ID) {
				continue
			}
			id := "cursor:" + thread.ID
			instance := map[string]any{"id": id, "agent": "cursor", "hostname": host, "contextId": thread.ID, "capabilities": map[string]any{"sessionControl": true, "history": true, "permission": false}}
			if directory := str(known[id]["directory"]); directory != "" {
				instance["directory"] = directory
			}
			hello := map[string]any{"type": "monitor.hello", "connection": "event", "instanceId": id, "instance": instance}
			c.state.observe(hello)
			if c.shouldForwardEvent(hello) {
				c.push(map[string]any{"type": "device.event", "event": hello})
			}
			session := map[string]any{"id": thread.ID, "agent": "cursor", "contextId": thread.ID, "title": thread.Title, "status": thread.Status, "updatedAt": thread.LastUpdatedAt}
			if directory := str(instance["directory"]); directory != "" {
				session["directory"] = directory
			}
			event := map[string]any{"type": "monitor.session", "instanceId": id, "session": session}
			c.state.observe(event)
			if c.shouldForwardEvent(event) {
				c.push(map[string]any{"type": "device.event", "event": event})
			}
		}
	}
	sync()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			sync()
		}
	}
}
