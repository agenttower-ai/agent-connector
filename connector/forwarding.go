package main

import (
	"errors"
	"sort"
)

func projectKey(agent, directory string) string { return recordKey(agent, directory) }

func (c *connector) loadForwarding() {
	config, err := readConfig(c.configPath)
	if err != nil {
		return
	}
	c.forwardingMu.Lock()
	defer c.forwardingMu.Unlock()
	for _, project := range config.DisabledProjects {
		if project.Agent != "" {
			c.disabledProjects[projectKey(project.Agent, project.Directory)] = project
		}
	}
}

func (c *connector) forwardingEnabled(agent, directory string) bool {
	if agent == "codebuddy-bridge" {
		return false
	}
	c.forwardingMu.RLock()
	defer c.forwardingMu.RUnlock()
	_, disabled := c.disabledProjects[projectKey(agent, directory)]
	return !disabled
}

func (c *connector) disabledProjectList() []projectRef {
	c.forwardingMu.RLock()
	defer c.forwardingMu.RUnlock()
	projects := make([]projectRef, 0, len(c.disabledProjects))
	for _, project := range c.disabledProjects {
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Agent == projects[j].Agent {
			return projects[i].Directory < projects[j].Directory
		}
		return projects[i].Agent < projects[j].Agent
	})
	return projects
}

func (c *connector) setForwarding(agent, directory string, enabled bool) error {
	if agent == "" || len(agent) > 100 || len(directory) > 4096 {
		return errors.New("项目标识无效")
	}
	return c.setForwardingBulk([]projectRef{{Agent: agent, Directory: directory}}, enabled)
}

func (c *connector) setForwardingBulk(requested []projectRef, enabled bool) error {
	if len(requested) == 0 || len(requested) > 2000 {
		return errors.New("请选择 1 到 2000 个项目")
	}
	projectsByKey := make(map[string]projectRef, len(requested))
	for _, project := range requested {
		if project.Agent == "" || len(project.Agent) > 100 || len(project.Directory) > 4096 || !c.state.hasProject(project.Agent, project.Directory) {
			return errors.New("部分项目已不存在，请刷新后重试")
		}
		projectsByKey[projectKey(project.Agent, project.Directory)] = project
	}
	config, err := readConfig(c.configPath)
	if err != nil {
		return err
	}
	c.forwardingMu.Lock()
	previous := make(map[string]projectRef, len(c.disabledProjects))
	for key, project := range c.disabledProjects {
		previous[key] = project
	}
	for key, project := range projectsByKey {
		if enabled {
			delete(c.disabledProjects, key)
		} else {
			c.disabledProjects[key] = project
		}
	}
	projects := make([]projectRef, 0, len(c.disabledProjects))
	for _, project := range c.disabledProjects {
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Agent == projects[j].Agent {
			return projects[i].Directory < projects[j].Directory
		}
		return projects[i].Agent < projects[j].Agent
	})
	config.DisabledProjects = projects
	if err := saveConfig(c.configPath, config); err != nil {
		c.disabledProjects = previous
		c.forwardingMu.Unlock()
		return err
	}
	c.forwardingMu.Unlock()
	c.push(map[string]any{"type": "device.snapshot", "state": c.forwardedSnapshot()})
	return nil
}

func (c *connector) forwardedSnapshot() map[string]any {
	snapshot := c.state.snapshot()
	allowed := map[string]bool{}
	instances := make([]any, 0)
	for _, value := range snapshot["instances"].([]any) {
		instance := obj(value)
		if c.forwardingEnabled(str(instance["agent"]), str(instance["directory"])) {
			allowed[str(instance["id"])] = true
			instances = append(instances, value)
		}
	}
	filter := func(values []any) []any {
		result := make([]any, 0)
		for _, value := range values {
			if allowed[str(obj(value)["instanceId"])] {
				result = append(result, value)
			}
		}
		return result
	}
	return map[string]any{
		"instances":   instances,
		"sessions":    filter(snapshot["sessions"].([]any)),
		"permissions": filter(snapshot["permissions"].([]any)),
	}
}

func (c *connector) shouldForwardEvent(event map[string]any) bool {
	if str(event["type"]) == "status" {
		return c.forwardingEnabled(str(event["agent"]), str(event["directory"]))
	}
	id := str(event["instanceId"])
	if instance := obj(event["instance"]); instance != nil {
		return c.forwardingEnabled(str(instance["agent"]), str(instance["directory"]))
	}
	agent, directory, ok := c.state.instanceProject(id)
	return ok && c.forwardingEnabled(agent, directory)
}
