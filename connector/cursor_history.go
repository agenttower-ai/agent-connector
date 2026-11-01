package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var cursorConversationID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func cursorHistory(sessionID string) ([]map[string]any, error) {
	if !cursorConversationID.MatchString(sessionID) {
		return nil, errors.New("无效的 Cursor 会话 ID")
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return nil, errors.New("无法定位 Cursor 会话目录")
	}
	root := filepath.Join(home, ".cursor", "projects")
	if override := os.Getenv("AGENTTOWER_CURSOR_PROJECTS_DIR"); override != "" {
		root = override
	}
	return cursorHistoryIn(root, sessionID)
}

func cursorHistoryIn(root, sessionID string) ([]map[string]any, error) {
	if !cursorConversationID.MatchString(sessionID) {
		return nil, errors.New("无效的 Cursor 会话 ID")
	}
	projects, e := os.ReadDir(root)
	if e != nil {
		return nil, errors.New("找不到本机 Cursor 会话记录")
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		path := filepath.Join(root, project.Name(), "agent-transcripts", sessionID, sessionID+".jsonl")
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > 32<<20 {
			return nil, errors.New("Cursor 会话记录过大")
		}
		return parseCursorTranscript(path, sessionID)
	}
	return nil, errors.New("找不到本机 Cursor 会话记录")
}

func parseCursorTranscript(path, sessionID string) ([]map[string]any, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, errors.New("无法读取 Cursor 会话记录")
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return nil, errors.New("无法读取 Cursor 会话记录")
	}
	messageTime := info.ModTime().UnixMilli()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	messages := []map[string]any{}
	line := 0
	for scanner.Scan() {
		line++
		var entry struct {
			Role    string `json:"role"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || (entry.Role != "user" && entry.Role != "assistant") {
			continue
		}
		parts := []string{}
		for _, part := range entry.Message.Content {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				parts = append(parts, part.Text)
			}
		}
		text := strings.TrimSpace(strings.Join(parts, "\n\n"))
		if text == "" {
			continue
		}
		if len(text) > 12000 {
			text = text[:12000]
		}
		messages = append(messages, map[string]any{"messageID": sessionID + ":" + strconv.Itoa(line), "role": entry.Role, "text": text, "at": messageTime + int64(line)})
		if len(messages) > 50 {
			messages = messages[1:]
		}
	}
	if scanner.Err() != nil {
		return nil, errors.New("Cursor 会话记录读取失败")
	}
	return messages, nil
}
