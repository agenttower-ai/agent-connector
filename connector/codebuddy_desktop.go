package main

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var codebuddyDesktopID = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

type codebuddyDesktopSession struct {
	ID, Directory, Title, Status string
	UpdatedAt                    int64
	HistoryDir                   string
}

func codebuddyDesktopRoot() string {
	if root := os.Getenv("AGENTTOWER_CODEBUDDY_DATA_DIR"); root != "" {
		return root
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "CodeBuddyExtension", "Data")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "CodeBuddyExtension", "Data")
}

func codebuddyWorkspaceHashes(roaming string) map[string]string {
	workspaces := map[string]string{}
	if roaming == "" {
		return workspaces
	}
	entries, _ := os.ReadDir(filepath.Join(roaming, "CodeBuddy CN", "User", "globalStorage", "tencent-cloud.coding-copilot", "genie-history"))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), "_") {
			continue
		}
		encoded := strings.TrimSuffix(entry.Name(), "_")
		path, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		directory := filepath.Clean(string(path))
		if runtime.GOOS == "windows" {
			directory = filepath.Clean(strings.ReplaceAll(directory, "/", `\`))
		}
		workspaces[codebuddyHash(directory)] = directory
	}
	return workspaces
}

func readCodebuddyJSON(path string, out any) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return errors.New("CodeBuddy 会话文件不可用")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// CodeBuddy stores some turns as a serialized chat envelope inside the message string.
// Keep only visible text parts; injected context is not part of the user's prompt.
func codebuddyMessageText(raw string) string {
	text := strings.TrimSpace(raw)
	var envelope struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal([]byte(text), &envelope) != nil || (envelope.Role != "user" && envelope.Role != "assistant") || envelope.Content == nil {
		return text
	}
	parts := []string{}
	for _, part := range envelope.Content {
		if part.Type != "text" {
			continue
		}
		value := strings.TrimSpace(part.Text)
		if envelope.Role == "user" {
			if start := strings.Index(value, "<user_query>"); start >= 0 {
				value = value[start+len("<user_query>"):]
				if end := strings.Index(value, "</user_query>"); end >= 0 {
					value = value[:end]
				}
			} else if start := strings.Index(value, "<additional_data>"); start >= 0 {
				if end := strings.Index(value[start:], "</additional_data>"); end >= 0 {
					value = value[:start] + value[start+end+len("</additional_data>"):]
				}
			}
		}
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n\n")
}

func codebuddyDesktopSessions(root string) ([]codebuddyDesktopSession, error) {
	accounts, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	rows := []codebuddyDesktopSession{}
	seen := map[string]bool{}
	workspaces := codebuddyWorkspaceHashes(os.Getenv("APPDATA"))
	workspaceFromHash := func(hash string) string { return workspaces[hash] }
	for _, account := range accounts {
		if !account.IsDir() || account.Name() == "Public" {
			continue
		}
		base := filepath.Join(root, account.Name(), "CodeBuddyIDE")
		profiles, _ := os.ReadDir(base)
		for _, profile := range profiles {
			if !profile.IsDir() {
				continue
			}
			histories, _ := os.ReadDir(filepath.Join(base, profile.Name(), "history"))
			for _, history := range histories {
				if !history.IsDir() || !codebuddyDesktopID.MatchString(history.Name()) {
					continue
				}
				folder := filepath.Join(base, profile.Name(), "history", history.Name())
				var index struct {
					Conversations []struct {
						ID            string `json:"id"`
						Name          string `json:"name"`
						LastMessageAt string `json:"lastMessageAt"`
					} `json:"conversations"`
				}
				if readCodebuddyJSON(filepath.Join(folder, "index.json"), &index) != nil {
					continue
				}
				for _, item := range index.Conversations {
					if !codebuddyDesktopID.MatchString(item.ID) || seen[item.ID] {
						continue
					}
					if info, err := os.Stat(filepath.Join(folder, item.ID, "index.json")); err != nil || !info.Mode().IsRegular() {
						continue
					}
					messages, err := codebuddyDesktopHistory(filepath.Join(folder, item.ID), item.ID)
					if err != nil || len(messages) == 0 {
						continue
					}
					updated, _ := time.Parse(time.RFC3339Nano, item.LastMessageAt)
					if directory := workspaceFromHash(history.Name()); directory != "" {
						seen[item.ID] = true
						rows = append(rows, codebuddyDesktopSession{ID: item.ID, Directory: directory, Title: item.Name, Status: "idle", UpdatedAt: updated.UnixMilli(), HistoryDir: filepath.Join(folder, item.ID)})
					}
				}
			}
		}
	}
	return rows, nil
}

func codebuddyDesktopHistory(dir, sessionID string) ([]map[string]any, error) {
	if !codebuddyDesktopID.MatchString(sessionID) || filepath.Base(dir) != sessionID {
		return nil, errors.New("无效 CodeBuddy 会话")
	}
	var index struct {
		Messages []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := readCodebuddyJSON(filepath.Join(dir, "index.json"), &index); err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, entry := range index.Messages {
		if entry.Role != "user" && entry.Role != "assistant" {
			continue
		}
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`).MatchString(entry.ID) {
			continue
		}
		var data struct {
			Role      string `json:"role"`
			Message   string `json:"message"`
			CreatedAt string `json:"createdAt"`
		}
		if readCodebuddyJSON(filepath.Join(dir, "messages", entry.ID+".json"), &data) != nil || data.Role != entry.Role {
			continue
		}
		text := codebuddyMessageText(data.Message)
		if text == "" {
			continue
		}
		if len(text) > 12000 {
			text = text[:12000]
		}
		at, _ := time.Parse(time.RFC3339Nano, data.CreatedAt)
		rows = append(rows, map[string]any{"messageID": entry.ID, "role": entry.Role, "text": text, "at": at.UnixMilli()})
		if len(rows) > 50 {
			rows = rows[1:]
		}
	}
	return rows, nil
}

func (c *connector) codebuddyDesktopLoop() {
	previous := map[string]string{}
	initialized := false
	sync := func() {
		rows, err := codebuddyDesktopSessions(codebuddyDesktopRoot())
		if err != nil {
			return
		}
		host, _ := os.Hostname()
		instances, sessions := map[string]map[string]any{}, map[string]map[string]any{}
		locations := map[string]codebuddyDesktopSession{}
		for _, row := range rows {
			if row.Directory == "" {
				continue
			}
			id := "codebuddy-desktop:" + row.ID
			c.mu.Lock()
			bridge := c.controls["codebuddy-bridge:"+codebuddyHash(row.Directory)]
			control := bridge != nil
			c.mu.Unlock()
			if control {
				control = false
				for _, value := range c.state.snapshot()["instances"].([]any) {
					instance := obj(value)
					seen, _ := instance["lastSeenAt"].(int64)
					if str(instance["id"]) == bridge.id && time.Now().UnixMilli()-seen < 15000 {
						control = true
					}
				}
			}
			instances[id] = map[string]any{"id": id, "agent": "codebuddy-desktop", "hostname": host, "directory": row.Directory, "contextId": row.ID, "online": true, "lastSeenAt": time.Now().UnixMilli(), "capabilities": map[string]any{"sessionControl": control, "history": true, "permission": false}}
			sessions[recordKey(id, row.ID)] = map[string]any{"id": row.ID, "instanceId": id, "agent": "codebuddy-desktop", "contextId": row.ID, "directory": row.Directory, "title": row.Title, "status": row.Status, "updatedAt": row.UpdatedAt}
			locations[row.ID] = row
			messages, err := codebuddyDesktopHistory(row.HistoryDir, row.ID)
			if err == nil {
				for _, message := range messages {
					key := recordKey(id, str(message["messageID"]))
					text := str(message["text"])
					if old, exists := previous[key]; initialized && (!exists || old != text) {
						typ := "monitor.output"
						if message["role"] == "user" {
							typ = "monitor.input"
						}
						event := map[string]any{"type": typ, "instanceId": id, "sessionID": row.ID, "messageID": message["messageID"], "text": text}
						if c.shouldForwardEvent(event) {
							c.push(map[string]any{"type": "device.event", "event": event})
						}
					}
					previous[key] = text
				}
			}
		}
		c.codebuddyMu.Lock()
		c.codebuddyLocations = locations
		c.state.replaceCodebuddyDesktop(instances, sessions)
		c.codebuddyMu.Unlock()
		initialized = true
		c.push(map[string]any{"type": "device.snapshot", "state": c.forwardedSnapshot()})
	}
	workspaceRoot := filepath.Join(os.Getenv("APPDATA"), "CodeBuddy CN", "User", "globalStorage", "tencent-cloud.coding-copilot", "genie-history")
	watchCodebuddyHistory(c.ctx, codebuddyDesktopRoot(), workspaceRoot, sync)
}

func (c *connector) codebuddySession(sessionID string) (codebuddyDesktopSession, bool) {
	c.codebuddyMu.RLock()
	defer c.codebuddyMu.RUnlock()
	row, ok := c.codebuddyLocations[sessionID]
	return row, ok
}

func codebuddyHash(directory string) string {
	sum := md5.Sum([]byte(filepath.Clean(directory)))
	return hex.EncodeToString(sum[:])
}
