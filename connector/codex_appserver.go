package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var codexThreadID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func codexExecutable() string {
	if v := os.Getenv("AGENTTOWER_CODEX_CLI"); v != "" {
		return v
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		if path := codexDesktopExecutable(filepath.Join(local, "OpenAI", "Codex", "bin")); path != "" {
			return path
		}
	}
	return "codex"
}

// Desktop updates install their CLI in a versioned subdirectory. The executable
// at bin/codex.exe can remain at an older version after an update.
func codexDesktopExecutable(bin string) string {
	entries, err := os.ReadDir(bin)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(bin, entry.Name(), "codex.exe")
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if newest == "" || info.ModTime().After(newestTime) {
			newest, newestTime = path, info.ModTime()
		}
	}
	if newest != "" {
		return newest
	}
	path := filepath.Join(bin, "codex.exe")
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path
	}
	return ""
}

type codexRPC struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	mu     sync.Mutex
	once   sync.Once
	closed atomic.Bool
}

func startCodexRPC() (*codexRPC, error) {
	cmd := exec.Command(codexExecutable(), "app-server")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	// The installed CLI can lag behind the desktop application's config schema.
	// A broken user config should be reported by Codex, not silently overwritten.
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	r := &codexRPC{cmd: cmd, in: in, out: scanner}
	if _, err = r.callWithin(1, "initialize", map[string]any{"clientInfo": map[string]any{"name": "agenttower", "version": "0.1"}, "capabilities": map[string]any{}}, 10*time.Second); err != nil {
		r.close()
		return nil, err
	}
	if err = r.send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		r.close()
		return nil, err
	}
	return r, nil
}

func (r *codexRPC) close() {
	r.once.Do(func() {
		r.closed.Store(true)
		r.in.Close()
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
			_ = r.cmd.Wait()
		}
	})
}

func (r *codexRPC) send(value any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		return io.ErrClosedPipe
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = r.in.Write(append(b, '\n'))
	return err
}

func (r *codexRPC) next() (map[string]any, error) {
	if !r.out.Scan() {
		if err := r.out.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	m := map[string]any{}
	if err := json.Unmarshal(r.out.Bytes(), &m); err != nil {
		return nil, err
	}
	if m["method"] != nil && m["id"] != nil {
		// The independent server cannot ask the user for tool approvals. Reject
		// server-initiated requests rather than leaving the turn hung forever.
		_ = r.send(map[string]any{"id": m["id"], "error": map[string]any{"code": -32000, "message": "请在 Codex 桌面端处理交互审批"}})
		return r.next()
	}
	return m, nil
}

func (r *codexRPC) call(id int, method string, params any) (map[string]any, error) {
	if err := r.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		m, err := r.next()
		if err != nil {
			return nil, err
		}
		if number(m["id"]) != float64(id) {
			continue
		}
		if problem := obj(m["error"]); problem != nil {
			return nil, errors.New(str(problem["message"]))
		}
		return obj(m["result"]), nil
	}
}

func (r *codexRPC) callWithin(id int, method string, params any, timeout time.Duration) (map[string]any, error) {
	timer := time.AfterFunc(timeout, r.close)
	defer timer.Stop()
	return r.call(id, method, params)
}

type codexIndexEntry struct {
	ID      string `json:"id"`
	Name    string `json:"thread_name"`
	Updated string `json:"updated_at"`
}

func codexIndex() map[string]codexIndexEntry {
	result := map[string]codexIndexEntry{}
	f, err := os.Open(filepath.Join(codexHome(), "session_index.jsonl"))
	if err != nil {
		return result
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		var item codexIndexEntry
		if json.Unmarshal(scanner.Bytes(), &item) == nil && codexThreadID.MatchString(item.ID) {
			result[item.ID] = item
		}
	}
	return result
}

func (c *connector) syncCodexThreads(projects []codexDesktopProject) {
	assigned, err := codexDesktopThreads()
	if err != nil {
		return
	}
	index := codexIndex()
	paths := map[string]string{}
	for _, project := range projects {
		paths[project.ID] = codexDesktopProjectDirectory(project)
	}
	// The relay accepts at most 1000 instances and sessions per snapshot.
	// Keep the most recently updated threads when the local archive is larger.
	type candidate struct {
		id, project string
		entry       codexIndexEntry
		updated     time.Time
	}
	selected := []candidate{}
	for thread, project := range assigned {
		if paths[project] == "" {
			continue
		}
		if _, err := codexRolloutFile(thread); err != nil {
			continue
		}
		entry := index[thread]
		at, _ := time.Parse(time.RFC3339Nano, entry.Updated)
		selected = append(selected, candidate{thread, project, entry, at})
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].updated.After(selected[j].updated) })
	limit := 900 - len(projects)
	if limit < 0 {
		limit = 0
	}
	if len(selected) > limit {
		selected = selected[:limit]
	}
	instances := map[string]map[string]any{}
	sessions := map[string]map[string]any{}
	host, _ := os.Hostname()
	for _, item := range selected {
		thread := item.id
		dir := paths[item.project]
		entry := item.entry
		id := "codex-thread:" + thread
		if entry.Name == "" {
			entry.Name = thread
		}
		updated := item.updated.UnixMilli()
		if item.updated.IsZero() {
			updated = time.Now().UnixMilli()
		}
		status := "idle"
		if c.codexBusy(thread) {
			status = "busy"
		}
		instances[id] = map[string]any{"id": id, "agent": "codex", "source": "desktop", "hostname": host, "directory": dir, "contextId": thread, "online": true, "lastSeenAt": time.Now().UnixMilli(), "capabilities": map[string]any{"sessionControl": true, "history": true, "permission": false}}
		sessions[recordKey(id, thread)] = map[string]any{"id": thread, "instanceId": id, "agent": "codex", "contextId": thread, "directory": dir, "title": entry.Name, "status": status, "updatedAt": updated}
	}
	c.state.replaceCodexThreads(instances, sessions)
}

func codexRolloutFile(id string) (string, error) {
	if !codexThreadID.MatchString(id) {
		return "", errors.New("无效 Codex 会话")
	}
	root := filepath.Join(codexHome(), "sessions")
	var file string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), "-"+id+".jsonl") {
			file = path
			return io.EOF
		}
		return nil
	})
	if file == "" {
		return "", errors.New("找不到 Codex 会话记录")
	}
	return file, nil
}

func codexHistory(id string) ([]any, error) {
	file, err := codexRolloutFile(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	messages := []any{}
	for scanner.Scan() {
		var event struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "response_item" || event.Payload.Type != "message" || (event.Payload.Role != "user" && event.Payload.Role != "assistant") {
			continue
		}
		parts := []string{}
		for _, part := range event.Payload.Content {
			if (part.Type == "input_text" || part.Type == "output_text") && part.Text != "" {
				parts = append(parts, part.Text)
			}
		}
		text := strings.Join(parts, "\n\n")
		if text == "" {
			continue
		}
		if len(text) > 12000 {
			text = text[:12000]
		}
		when, _ := time.Parse(time.RFC3339Nano, event.Timestamp)
		messages = append(messages, map[string]any{"messageID": fmt.Sprintf("%s:%d", id, len(messages)), "role": event.Payload.Role, "text": text, "at": when.UnixMilli()})
		if len(messages) > 50 {
			messages = messages[1:]
		}
	}
	return messages, scanner.Err()
}

func (c *connector) codexBusy(id string) bool {
	c.codexMu.Lock()
	defer c.codexMu.Unlock()
	return c.codexRunning[id]
}

func (c *connector) codexPrompt(id, text string) error {
	if !codexThreadID.MatchString(id) || strings.TrimSpace(text) == "" || len(text) > 4000 {
		return errors.New("无效 Codex 会话或消息")
	}
	// Verify the requested thread still belongs to the locally discovered project.
	assigned, err := codexDesktopThreads()
	if err != nil || assigned[id] == "" {
		return errors.New("Codex 会话已失效，请刷新")
	}
	if _, err := codexRolloutFile(id); err != nil {
		return err
	}
	c.codexMu.Lock()
	if c.codexRunning[id] {
		c.codexMu.Unlock()
		return errors.New("该 Codex 会话正在执行，请稍后重试")
	}
	c.codexRunning[id] = true
	c.codexMu.Unlock()
	keepRunning := false
	defer func() {
		if !keepRunning {
			c.finishCodexTurn(id)
		}
	}()
	r, err := startCodexRPC()
	if err != nil {
		return err
	}
	// Keep the process alive until the turn finishes; otherwise turn/start's
	// acknowledgement would be followed by an immediate cancellation.
	deadline := time.AfterFunc(10*time.Minute, r.close)
	resumed, err := r.callWithin(2, "thread/resume", map[string]any{"threadId": id}, 10*time.Second)
	if err != nil {
		deadline.Stop()
		r.close()
		return err
	}
	if str(obj(obj(resumed["thread"])["status"])["type"]) != "idle" {
		deadline.Stop()
		r.close()
		return errors.New("Codex 会话正在运行，请稍后重试")
	}
	_, err = r.callWithin(3, "turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": text}}}, 10*time.Second)
	if err != nil {
		deadline.Stop()
		r.close()
		return err
	}
	keepRunning = true
	go func() {
		defer deadline.Stop()
		defer r.close()
		defer c.finishCodexTurn(id)
		c.syncCodexThreadsFromDisk()
		for {
			m, err := r.next()
			if err != nil {
				break
			}
			if str(m["method"]) == "turn/completed" {
				break
			}
		}
	}()
	return nil
}

func (c *connector) finishCodexTurn(id string) {
	c.codexMu.Lock()
	delete(c.codexRunning, id)
	c.codexMu.Unlock()
	c.syncCodexThreadsFromDisk()
}

func (c *connector) syncCodexThreadsFromDisk() {
	projects, err := codexDesktopProjects()
	if err == nil {
		c.syncCodexThreads(projects)
	}
}
