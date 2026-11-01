package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type peer struct {
	conn                      *websocket.Conn
	mu                        sync.Mutex
	id, originalID, contextID string
}

func (p *peer) send(v any) error { p.mu.Lock(); defer p.mu.Unlock(); return p.conn.WriteJSON(v) }

type connector struct {
	state                *state
	mu                   sync.Mutex
	controls             map[string]*peer
	codexMu              sync.Mutex
	codexRunning         map[string]bool
	codebuddyMu          sync.RWMutex
	codebuddyLocations   map[string]codebuddyDesktopSession
	peers                map[*peer]bool
	listener             net.Listener
	server               *http.Server
	remote               *peer
	relayReady           atomic.Bool
	unbinding            atomic.Bool
	ctx                  context.Context
	cancel               context.CancelFunc
	url, token, deviceID string
	configPath           string
	forwardingMu         sync.RWMutex
	disabledProjects     map[string]projectRef
	bindingMu            sync.Mutex
	binding              *localBinding
}

func validRelay(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil {
		return false
	}
	return u.Scheme == "wss" || (u.Scheme == "ws" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))
}
func startConnector(raw, token, deviceID string, port int, configPath string) (*connector, error) {
	if !validRelay(raw) {
		return nil, errors.New("远程服务器必须使用 wss://")
	}
	if token == "" || deviceID == "" {
		return nil, errors.New("设备凭证缺失")
	}
	listener, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &connector{state: newState(), controls: map[string]*peer{}, codexRunning: map[string]bool{}, codebuddyLocations: map[string]codebuddyDesktopSession{}, peers: map[*peer]bool{}, listener: listener, ctx: ctx, cancel: cancel, url: raw, token: token, deviceID: deviceID, configPath: configPath, disabledProjects: map[string]projectRef{}}
	c.loadForwarding()
	c.server = &http.Server{Handler: http.HandlerFunc(c.accept)}
	go c.server.Serve(listener)
	go c.relayLoop()
	go c.cursorBridgeLoop()
	go c.codexDesktopLoop()
	go c.codebuddyDesktopLoop()
	return c, nil
}
func (c *connector) close() {
	c.cancel()
	c.server.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := range c.peers {
		p.conn.Close()
	}
	if c.remote != nil {
		c.remote.conn.Close()
	}
}
func (c *connector) accept(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "websocket" {
		c.manageHTTP(w, r)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "browser connections forbidden", http.StatusForbidden)
		return
	}
	u := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, e := u.Upgrade(w, r, nil)
	if e != nil {
		return
	}
	conn.SetReadLimit(1024 * 1024)
	p := &peer{conn: conn}
	c.mu.Lock()
	c.peers[p] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.peers, p)
		if c.controls[p.id] == p {
			delete(c.controls, p.id)
			c.state.disconnect(p.id)
		}
		c.mu.Unlock()
		conn.Close()
	}()
	for {
		var m map[string]any
		if e := conn.ReadJSON(&m); e != nil {
			return
		}
		if !c.localMessage(p, m) {
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "invalid event"), time.Now().Add(time.Second))
			return
		}
	}
}
func (c *connector) localMessage(p *peer, m map[string]any) bool {
	typ := str(m["type"])
	id := str(m["instanceId"])
	if typ == "monitor.action.result" && strings.HasPrefix(p.id, "codebuddy-bridge:") && id == p.originalID {
		target := str(m["targetInstanceId"])
		row, ok := c.codebuddySession(str(m["sessionID"]))
		if !ok || target != "codebuddy-desktop:"+row.ID || p.id != "codebuddy-bridge:"+codebuddyHash(row.Directory) {
			return true
		}
		m["instanceId"] = target
		delete(m, "targetInstanceId")
		if c.shouldForwardEvent(m) {
			c.push(map[string]any{"type": "device.event", "event": m})
		}
		return true
	}
	if typ == "monitor.hello" && str(obj(m["instance"])["id"]) == id {
		p.originalID = id
		p.id = id
		contextID := str(m["contextId"])
		if contextID == "" {
			contextID = str(obj(m["instance"])["contextId"])
		}
		if strings.HasPrefix(id, "opencode:") && strings.Count(id, ":") == 1 && contextID != "" {
			b, _ := json.Marshal([]any{contextID, obj(m["instance"])["pid"]})
			hash := sha256.Sum256(b)
			p.id = id + ":" + hex.EncodeToString(hash[:8])
		}
	}
	if p.id != "" && id == p.originalID {
		m["instanceId"] = p.id
		if instance := obj(m["instance"]); instance != nil {
			instance["id"] = p.id
		}
	}
	if typ == "monitor.hello" && str(m["connection"]) == "control" && str(obj(m["instance"])["id"]) == str(m["instanceId"]) {
		p.contextID = str(m["contextId"])
		if p.contextID == "" {
			p.contextID = str(obj(m["instance"])["contextId"])
		}
		c.mu.Lock()
		old := c.controls[p.id]
		c.controls[p.id] = p
		c.mu.Unlock()
		if old != nil && old != p {
			old.conn.Close()
		}
	}
	if p.id != "" && str(m["instanceId"]) != p.id {
		return true
	}
	agent, directory := c.state.memoryProject(p.id)
	if c.state.permissionMemory(m) && c.forwardingEnabled(agent, directory) {
		if p.send(map[string]any{"type": "control.permission.reply", "requestID": obj(m["request"])["id"], "reply": "once"}) == nil {
			return true
		}
	}
	if c.state.observe(m) {
		if c.shouldForwardEvent(m) {
			c.push(map[string]any{"type": "device.event", "event": m})
		}
		if typ == "monitor.action.result" && m["ok"] == true {
			c.push(map[string]any{"type": "device.snapshot", "state": c.forwardedSnapshot()})
		}
	}
	return true
}
func (c *connector) sendLocal(id string, m map[string]any) bool {
	c.mu.Lock()
	p := c.controls[id]
	c.mu.Unlock()
	if p == nil || (str(m["contextId"]) != "" && str(m["contextId"]) != p.contextID) {
		return false
	}
	n := clone(m)
	n["instanceId"] = p.originalID
	return p.send(n) == nil
}
func (c *connector) push(m map[string]any) {
	c.mu.Lock()
	p := c.remote
	c.mu.Unlock()
	if p != nil {
		_ = p.send(m)
	}
}
func (c *connector) relayLoop() {
	backoff := time.Second
	for c.ctx.Err() == nil {
		dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
		conn, _, e := dialer.DialContext(c.ctx, c.url, nil)
		if e == nil {
			p := &peer{conn: conn}
			c.mu.Lock()
			c.remote = p
			c.mu.Unlock()
			e = c.readRelay(p, &backoff)
			c.mu.Lock()
			if c.remote == p {
				c.remote = nil
				c.relayReady.Store(false)
			}
			c.mu.Unlock()
			conn.Close()
		}
		if c.ctx.Err() != nil {
			return
		}
		if e != nil {
			log.Printf("[relay] %v", e)
		}
		select {
		case <-time.After(backoff):
		case <-c.ctx.Done():
			return
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}
func (c *connector) readRelay(p *peer, backoff *time.Duration) error {
	p.conn.SetReadLimit(2 * 1024 * 1024)
	p.conn.SetPingHandler(func(data string) error {
		p.conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		return p.sendControl(websocket.PongMessage, []byte(data))
	})
	if e := p.send(map[string]any{"type": "device.auth", "token": c.token, "deviceId": c.deviceID}); e != nil {
		return e
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	done := make(chan error, 1)
	var ready atomic.Bool
	go func() {
		for {
			var m map[string]any
			if e := p.conn.ReadJSON(&m); e != nil {
				done <- e
				return
			}
			switch str(m["type"]) {
			case "device.ready":
				ready.Store(true)
				c.relayReady.Store(true)
				*backoff = time.Second
				c.push(map[string]any{"type": "device.snapshot", "state": c.forwardedSnapshot()})
			case "control.session":
				c.handleSession(m)
			case "control.permission.reply":
				c.handlePermission(m)
			}
		}
	}()
	for {
		select {
		case <-c.ctx.Done():
			return nil
		case e := <-done:
			if closeErr, ok := e.(*websocket.CloseError); ok && closeErr.Code == 1008 && (closeErr.Text == "device unbound" || closeErr.Text == "invalid device credential") {
				if c.unbinding.Load() {
					return nil
				}
				fmt.Println("[AgentTower] 设备凭证已撤销，Connector 停止连接。")
				c.cancel()
				c.server.Close()
				return nil
			}
			return e
		case <-ticker.C:
			if ready.Load() {
				c.push(map[string]any{"type": "device.snapshot", "state": c.forwardedSnapshot()})
			}
		}
	}
}
func (p *peer) sendControl(kind int, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn.WriteControl(kind, data, time.Now().Add(time.Second))
}
func (c *connector) eventError(instance, field, id, message string) {
	c.push(map[string]any{"type": "device.event", "event": map[string]any{"type": "monitor.control.error", "instanceId": instance, field: id, "error": message}})
}
func (c *connector) handleSession(m map[string]any) {
	id := str(m["instanceId"])
	action := str(m["action"])
	if agent, directory, ok := c.state.instanceProject(id); !ok || !c.forwardingEnabled(agent, directory) {
		c.eventError(id, "actionID", str(m["actionID"]), "此项目已在 Connector 中停止转发")
		return
	}
	snap := c.state.snapshot()
	contextID := ""
	agent := ""
	for _, v := range snap["instances"].([]any) {
		if str(obj(v)["id"]) == id {
			agent = str(obj(v)["agent"])
			break
		}
	}
	if action == "create" || action == "models" {
		for _, v := range snap["instances"].([]any) {
			if str(obj(v)["id"]) == id {
				contextID = str(obj(v)["contextId"])
			}
		}
	} else {
		for _, v := range snap["sessions"].([]any) {
			r := obj(v)
			if str(r["instanceId"]) == id && str(r["id"]) == str(m["sessionID"]) {
				contextID = str(r["contextId"])
			}
		}
	}
	if action == "history" && agent == "cursor" && contextID != "" && contextID == str(m["contextId"]) {
		messages, e := cursorHistory(str(m["sessionID"]))
		result := map[string]any{"type": "monitor.action.result", "instanceId": id, "actionID": m["actionID"], "action": action, "sessionID": m["sessionID"], "ok": e == nil, "messages": messages}
		if e != nil {
			result["error"] = e.Error()
		}
		c.push(map[string]any{"type": "device.event", "event": result})
		return
	}
	if action == "prompt" && agent == "cursor" && contextID != "" && contextID == str(m["contextId"]) {
		err := cursorDesktopSend(str(m["sessionID"]), str(m["text"]))
		result := map[string]any{"type": "monitor.action.result", "instanceId": id, "actionID": m["actionID"], "action": action, "sessionID": m["sessionID"], "ok": err == nil}
		if err != nil {
			result["error"] = err.Error()
		}
		c.push(map[string]any{"type": "device.event", "event": result})
		return
	}
	if agent == "codebuddy-desktop" && contextID != "" && contextID == str(m["contextId"]) {
		row, ok := c.codebuddySession(str(m["sessionID"]))
		result := map[string]any{"type": "monitor.action.result", "instanceId": id, "actionID": m["actionID"], "action": action, "sessionID": m["sessionID"], "ok": false}
		if !ok || "codebuddy-desktop:"+row.ID != id {
			result["error"] = "CodeBuddy 会话已失效，请刷新"
		} else if action == "history" {
			messages, err := codebuddyDesktopHistory(row.HistoryDir, row.ID)
			result["ok"] = err == nil
			result["messages"] = messages
			if err != nil {
				result["error"] = err.Error()
			}
		} else if action == "prompt" {
			request := clone(m)
			request["contextId"] = codebuddyHash(row.Directory)
			request["targetInstanceId"] = id
			if c.sendLocal("codebuddy-bridge:"+codebuddyHash(row.Directory), request) {
				return
			}
			result["error"] = "CodeBuddy 桌面桥未连接，请重启 IDE 并打开目标工作区"
		} else {
			result["error"] = "CodeBuddy 桌面会话暂不支持此操作"
		}
		c.push(map[string]any{"type": "device.event", "event": result})
		return
	}
	if strings.HasPrefix(id, "codex-thread:") && contextID != "" && contextID == str(m["contextId"]) {
		var result map[string]any
		switch action {
		case "history":
			messages, err := codexHistory(str(m["sessionID"]))
			result = map[string]any{"type": "monitor.action.result", "instanceId": id, "actionID": m["actionID"], "action": action, "sessionID": m["sessionID"], "ok": err == nil, "messages": messages}
			if err != nil {
				result["error"] = err.Error()
			}
		case "prompt":
			err := c.codexPrompt(str(m["sessionID"]), str(m["text"]))
			result = map[string]any{"type": "monitor.action.result", "instanceId": id, "actionID": m["actionID"], "action": action, "sessionID": m["sessionID"], "ok": err == nil}
			if err != nil {
				result["error"] = err.Error()
			}
		default:
			result = map[string]any{"type": "monitor.action.result", "instanceId": id, "actionID": m["actionID"], "ok": false, "error": "Codex 暂不支持此操作"}
		}
		c.push(map[string]any{"type": "device.event", "event": result})
		return
	}
	if (action != "create" && action != "abort" && action != "prompt" && action != "history" && action != "models") || contextID == "" || contextID != str(m["contextId"]) || !c.sendLocal(id, m) {
		c.push(map[string]any{"type": "device.event", "event": map[string]any{"type": "monitor.action.result", "instanceId": id, "actionID": m["actionID"], "ok": false, "error": "会话控制通道已失效，请更新适配器并重启 Agent"}})
	}
}
func (c *connector) handlePermission(m map[string]any) {
	id := str(m["instanceId"])
	if agent, directory, ok := c.state.instanceProject(id); !ok || !c.forwardingEnabled(agent, directory) {
		return
	}
	requestID := str(m["requestID"])
	reply := str(m["reply"])
	valid := reply == "once" || reply == "always" || reply == "reject"
	if reply == "answer" {
		if answers, ok := m["answers"].([]any); ok && len(answers) <= 5 {
			valid = true
			for _, answer := range answers {
				row, ok := answer.([]any)
				if !ok || len(row) > 20 {
					valid = false
					break
				}
				for _, v := range row {
					s, ok := v.(string)
					if !ok || len(s) > 2000 {
						valid = false
					}
				}
			}
		}
	}
	if !valid {
		return
	}
	if reply == "always" && c.state.prepareRememberedReply(id, requestID) {
		reply = "once"
	}
	exists := false
	for _, v := range c.state.snapshot()["permissions"].([]any) {
		r := obj(v)
		if str(r["instanceId"]) == id && str(r["id"]) == requestID {
			exists = true
		}
	}
	if !exists || !c.sendLocal(id, map[string]any{"type": "control.permission.reply", "requestID": requestID, "reply": reply, "answers": m["answers"]}) {
		c.eventError(id, "requestID", requestID, "设备上的审批或控制通道已失效")
	}
}
