package main

import "encoding/json"

func rememberedAgent(agent string) bool {
	return agent == "claude" || agent == "cursor" || agent == "codebuddy" || agent == "DeepSeek Harness"
}

// Match the complete request within its runtime, project and session. Never infer
// command prefixes or wildcard permissions from a hook's display text.
func permissionMemoryKey(instance, request map[string]any) string {
	if !rememberedAgent(str(instance["agent"])) || request["allowSimilar"] == false || str(request["sessionID"]) == "" || str(request["permission"]) == "" || request["patterns"] == nil || request["questions"] != nil {
		return ""
	}
	b, err := json.Marshal([]any{instance["id"], instance["directory"], request["directory"], request["sessionID"], request["permission"], request["patterns"], request["metadata"]})
	if err != nil {
		return ""
	}
	return string(b)
}

func (s *state) permissionMemory(m map[string]any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, requestID := str(m["instanceId"]), str(m["requestID"])
	if str(m["type"]) == "monitor.permission.resolved" {
		key := recordKey(id, requestID)
		if grant := s.pendingGrants[key]; grant != "" {
			if str(m["reply"]) == "once" {
				s.grants[grant] = true
			}
			delete(s.pendingGrants, key)
		}
	}
	if str(m["type"]) == "monitor.control.error" {
		delete(s.pendingGrants, recordKey(id, requestID))
	}
	if str(m["type"]) != "monitor.permission" {
		return false
	}
	r := obj(m["request"])
	key := permissionMemoryKey(s.instances[id], r)
	if key == "" {
		return false
	}
	r["allowSimilar"] = true
	return s.grants[key]
}

func (s *state) prepareRememberedReply(id, requestID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := permissionMemoryKey(s.instances[id], s.permissions[recordKey(id, requestID)])
	if key == "" {
		return false
	}
	s.pendingGrants[recordKey(id, requestID)] = key
	return true
}

func (s *state) memoryProject(id string) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return str(s.instances[id]["agent"]), str(s.instances[id]["directory"])
}
