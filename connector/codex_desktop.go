package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type codexDesktopProject struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	RootPaths []string `json:"rootPaths"`
}

type codexDesktopState struct {
	LocalProjects  map[string]codexDesktopProject `json:"local-projects"`
	ProjectOrder   []string                       `json:"project-order"`
	ThreadProjects map[string]struct {
		ProjectID string `json:"projectId"`
	} `json:"thread-project-assignments"`
}

func codexHome() string {
	if value := os.Getenv("CODEX_HOME"); value != "" {
		return value
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

func codexDesktopStateFile() string {
	return filepath.Join(codexHome(), ".codex-global-state.json")
}

func codexDesktopProjectsFrom(path string) ([]codexDesktopProject, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state codexDesktopState
	if err = json.Unmarshal(b, &state); err != nil {
		return nil, errors.New("Codex 桌面端项目状态文件无效")
	}
	ordered := make([]codexDesktopProject, 0, len(state.LocalProjects))
	seen := map[string]bool{}
	add := func(id string) {
		project, ok := state.LocalProjects[id]
		if !ok || seen[id] || project.ID != id || project.ID == "" || codexDesktopProjectDirectory(project) == "" {
			return
		}
		seen[id] = true
		ordered = append(ordered, project)
	}
	for _, id := range state.ProjectOrder {
		add(id)
	}
	rest := make([]string, 0, len(state.LocalProjects))
	for id := range state.LocalProjects {
		rest = append(rest, id)
	}
	sort.Strings(rest)
	for _, id := range rest {
		add(id)
	}
	return ordered, nil
}

func codexDesktopProjects() ([]codexDesktopProject, error) {
	return codexDesktopProjectsFrom(codexDesktopStateFile())
}

func codexDesktopThreads() (map[string]string, error) {
	b, err := os.ReadFile(codexDesktopStateFile())
	if err != nil {
		return nil, err
	}
	var state codexDesktopState
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for id, assignment := range state.ThreadProjects {
		if project, ok := state.LocalProjects[assignment.ProjectID]; codexThreadID.MatchString(id) && ok && project.ID == assignment.ProjectID {
			ids[id] = assignment.ProjectID
		}
	}
	return ids, nil
}

func codexDesktopProjectDirectory(project codexDesktopProject) string {
	for _, root := range project.RootPaths {
		root = strings.TrimPrefix(root, `\\?\`)
		if filepath.IsAbs(root) {
			if info, err := os.Stat(root); err == nil && info.IsDir() {
				return filepath.Clean(root)
			}
		}
	}
	return ""
}

func codexDesktopInstance(project codexDesktopProject, host string, now int64) (map[string]any, map[string]any) {
	id := "codex-desktop:" + project.ID
	directory := codexDesktopProjectDirectory(project)
	instance := map[string]any{
		"id": id, "agent": "codex", "source": "desktop", "hostname": host,
		"contextId": project.ID, "online": true, "lastSeenAt": now,
		"capabilities": map[string]any{"sessionControl": false, "permission": false, "projectDiscovery": true},
	}
	if directory != "" {
		instance["directory"] = directory
	}
	session := map[string]any{
		"id": project.ID, "agent": "codex", "contextId": project.ID,
		"title": project.Name, "status": "idle", "updatedAt": now,
	}
	if directory != "" {
		session["directory"] = directory
	}
	return instance, session
}

func (c *connector) codexDesktopLoop() {
	sync := func() {
		projects, err := codexDesktopProjects()
		if err != nil {
			return
		}
		host, _ := os.Hostname()
		now := time.Now().UnixMilli()
		instances := make(map[string]map[string]any, len(projects))
		sessions := make(map[string]map[string]any, len(projects))
		for _, project := range projects {
			instance, session := codexDesktopInstance(project, host, now)
			id := str(instance["id"])
			session["instanceId"] = id
			instances[id] = instance
			sessions[recordKey(id, str(session["id"]))] = session
		}
		c.state.replaceCodexDesktop(instances, sessions)
		c.syncCodexThreads(projects)
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
