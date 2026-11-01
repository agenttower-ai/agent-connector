package main

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

type state struct {
	mu            sync.Mutex
	instances     map[string]map[string]any
	sessions      map[string]map[string]any
	permissions   map[string]map[string]any
	grants        map[string]bool
	pendingGrants map[string]string
}

func newState() *state {
	return &state{instances: map[string]map[string]any{}, sessions: map[string]map[string]any{}, permissions: map[string]map[string]any{}, grants: map[string]bool{}, pendingGrants: map[string]string{}}
}
func recordKey(id, value string) string { b, _ := json.Marshal([]string{id, value}); return string(b) }
func str(v any) string                  { s, _ := v.(string); return s }
func obj(v any) map[string]any          { m, _ := v.(map[string]any); return m }
func clone(m map[string]any) map[string]any {
	n := map[string]any{}
	for k, v := range m {
		n[k] = v
	}
	return n
}
func (s *state) observe(m map[string]any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	typ := str(m["type"])
	id := str(m["instanceId"])
	now := time.Now().UnixMilli()
	if typ == "status" && str(m["agent"]) != "opencode" && str(m["agent"]) != "" {
		id = "hook:" + str(m["agent"]) + ":" + str(m["sessionID"])
		if str(m["sessionID"]) == "" {
			id = "hook:" + str(m["agent"]) + ":aggregate"
		}
		host, _ := os.Hostname()
		s.instances[id] = map[string]any{"id": id, "agent": m["agent"], "hostname": host, "online": true, "lastSeenAt": now, "capabilities": map[string]any{"sessionControl": false}}
		status := m["value"]
		if status == "success" {
			status = "idle"
		}
		s.sessions[recordKey(id, "status")] = map[string]any{"id": "status", "instanceId": id, "agent": m["agent"], "title": m["title"], "status": status, "updatedAt": now}
		return true
	}
	if id == "" {
		return false
	}
	if typ == "monitor.hello" && str(obj(m["instance"])["id"]) == id {
		v := clone(obj(m["instance"]))
		if str(v["agent"]) == "cursor" {
			// Desktop Bridge discovery may already have established control for
			// this conversation. A subsequent hook must not erase that capability.
			if old := s.instances[id]; old != nil && obj(old["capabilities"])["sessionControl"] == true {
				capabilities := clone(obj(v["capabilities"]))
				capabilities["sessionControl"] = true
				v["capabilities"] = capabilities
			}
		}
		v["online"] = true
		v["lastSeenAt"] = now
		s.instances[id] = v
		m["instance"] = v
	}
	instance, ok := s.instances[id]
	if !ok {
		return false
	}
	instance["online"] = true
	instance["lastSeenAt"] = now
	if str(instance["agent"]) == "codebuddy-bridge" {
		for desktopID, desktop := range s.instances {
			if strings.HasPrefix(desktopID, "codebuddy-desktop:") && str(desktop["directory"]) == str(instance["directory"]) {
				obj(desktop["capabilities"])["sessionControl"] = true
				desktop["lastSeenAt"] = now
			}
		}
	}
	if typ == "monitor.snapshot" {
		sessions, ok1 := m["sessions"].([]any)
		permissions, ok2 := m["permissions"].([]any)
		if !ok1 || !ok2 {
			return false
		}
		for k, v := range s.sessions {
			if str(v["instanceId"]) == id {
				delete(s.sessions, k)
			}
		}
		for k, v := range s.permissions {
			if str(v["instanceId"]) == id {
				delete(s.permissions, k)
			}
		}
		for _, v := range sessions {
			r := obj(v)
			if str(r["id"]) != "" {
				n := clone(r)
				n["instanceId"] = id
				s.sessions[recordKey(id, str(r["id"]))] = n
			}
		}
		for _, v := range permissions {
			r := obj(v)
			if str(r["id"]) != "" {
				n := clone(r)
				n["instanceId"] = id
				s.permissions[recordKey(id, str(r["id"]))] = n
			}
		}
	}
	if typ == "monitor.session" {
		r := obj(m["session"])
		if str(r["id"]) != "" {
			n := clone(r)
			n["instanceId"] = id
			key := recordKey(id, str(r["id"]))
			if previous := s.sessions[key]; str(n["agent"]) == "cursor" && previous != nil && str(n["title"]) == str(n["directory"]) && str(previous["title"]) != "" && str(previous["title"]) != str(previous["directory"]) {
				n["title"] = previous["title"]
			}
			s.sessions[key] = n
		}
	}
	if typ == "monitor.session.remove" {
		delete(s.sessions, recordKey(id, str(m["sessionID"])))
	}
	if typ == "monitor.permission" {
		r := obj(m["request"])
		if str(r["id"]) != "" {
			n := clone(r)
			n["instanceId"] = id
			s.permissions[recordKey(id, str(r["id"]))] = n
		}
	}
	if typ == "monitor.permission.resolved" {
		delete(s.permissions, recordKey(id, str(m["requestID"])))
	}
	if typ == "monitor.control.error" {
		if p := s.permissions[recordKey(id, str(m["requestID"]))]; p != nil {
			p["error"] = m["error"]
		}
	}
	return true
}
func (s *state) disconnect(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bridge := s.instances[id]; str(bridge["agent"]) == "codebuddy-bridge" {
		for desktopID, desktop := range s.instances {
			if strings.HasPrefix(desktopID, "codebuddy-desktop:") && str(desktop["directory"]) == str(bridge["directory"]) {
				obj(desktop["capabilities"])["sessionControl"] = false
			}
		}
	}
	delete(s.instances, id)
	for k, v := range s.sessions {
		if str(v["instanceId"]) == id {
			delete(s.sessions, k)
		}
	}
	for k, v := range s.permissions {
		if str(v["instanceId"]) == id {
			delete(s.permissions, k)
		}
	}
}
func (s *state) instanceProject(id string) (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	instance, ok := s.instances[id]
	if !ok {
		return "", "", false
	}
	return str(instance["agent"]), str(instance["directory"]), true
}
func (s *state) hasProject(agent, directory string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, instance := range s.instances {
		if str(instance["agent"]) == agent && str(instance["directory"]) == directory {
			return true
		}
	}
	return false
}
func (s *state) replaceCodexDesktop(instances, sessions map[string]map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.instances {
		if strings.HasPrefix(id, "codex-desktop:") {
			delete(s.instances, id)
		}
	}
	for key, session := range s.sessions {
		if strings.HasPrefix(str(session["instanceId"]), "codex-desktop:") {
			delete(s.sessions, key)
		}
	}
	for id, instance := range instances {
		s.instances[id] = instance
	}
	for key, session := range sessions {
		s.sessions[key] = session
	}
}
func (s *state) replaceCodexThreads(instances, sessions map[string]map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.instances {
		if strings.HasPrefix(id, "codex-thread:") {
			delete(s.instances, id)
		}
	}
	for key, session := range s.sessions {
		if strings.HasPrefix(str(session["instanceId"]), "codex-thread:") {
			delete(s.sessions, key)
		}
	}
	for id, instance := range instances {
		s.instances[id] = instance
	}
	for key, session := range sessions {
		s.sessions[key] = session
	}
}
func (s *state) replaceCodebuddyDesktop(instances, sessions map[string]map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.instances {
		if strings.HasPrefix(id, "codebuddy-desktop:") {
			delete(s.instances, id)
		}
	}
	for key, session := range s.sessions {
		if strings.HasPrefix(str(session["instanceId"]), "codebuddy-desktop:") {
			delete(s.sessions, key)
		}
	}
	for id, instance := range instances {
		s.instances[id] = instance
	}
	for key, session := range sessions {
		s.sessions[key] = session
	}
}
func (s *state) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := []any{}
	b := []any{}
	c := []any{}
	for _, v := range s.instances {
		a = append(a, clone(v))
	}
	for _, v := range s.sessions {
		b = append(b, clone(v))
	}
	for _, v := range s.permissions {
		c = append(c, clone(v))
	}
	return map[string]any{"instances": a, "sessions": b, "permissions": c}
}
