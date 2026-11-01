package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func (c *connector) manageHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != c.listener.Addr().String() && r.Host != strings.Replace(c.listener.Addr().String(), "127.0.0.1", "localhost", 1) {
		http.Error(w, "invalid host", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost {
		origin := r.Header.Get("Origin")
		u, err := url.Parse(origin)
		if err != nil || (origin != "" && (u.Scheme != "http" || u.Host != r.Host)) || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "invalid origin or content type", http.StatusForbidden)
			return
		}
	}
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(managementHTML))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/status":
			c.writeJSON(w, http.StatusOK, c.managementStatus())
		case r.Method == http.MethodPost && r.URL.Path == "/api/binding/new":
			c.newBinding(w)
		case r.Method == http.MethodGet && r.URL.Path == "/api/binding/status":
			c.bindingStatus(w)
		case r.Method == http.MethodGet && r.URL.Path == "/api/bindings":
			c.bindings(w, "", false)
		case r.Method == http.MethodPost && r.URL.Path == "/api/forwarding":
			var body struct {
				Agent     string `json:"agent"`
				Directory string `json:"directory"`
				Enabled   *bool  `json:"enabled"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body) != nil || body.Enabled == nil {
				c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式无效"})
				return
			}
			if err := c.setForwarding(body.Agent, body.Directory, *body.Enabled); err != nil {
				c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			c.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": *body.Enabled})
		case r.Method == http.MethodPost && r.URL.Path == "/api/forwarding/bulk":
			var body struct {
				Projects []projectRef `json:"projects"`
				Enabled  *bool        `json:"enabled"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body) != nil || body.Enabled == nil {
				c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式无效"})
				return
			}
			if err := c.setForwardingBulk(body.Projects, *body.Enabled); err != nil {
				c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			c.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": *body.Enabled, "count": len(body.Projects)})
		case r.Method == http.MethodPost && r.URL.Path == "/api/bindings/remove":
			var body struct {
				PhoneID string `json:"phoneId"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil || body.PhoneID == "" {
				c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "手机标识无效"})
				return
			}
			c.bindings(w, body.PhoneID, true)
		case r.Method == http.MethodPost && r.URL.Path == "/api/install":
			var body struct{ Agent string }
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body) != nil {
				c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式无效"})
				return
			}
			if body.Agent != "opencode" && body.Agent != "claude" && body.Agent != "codebuddy" && body.Agent != "codebuddy-desktop" && body.Agent != "cursor" && body.Agent != "codex" {
				c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不支持的 Agent"})
				return
			}
			if err := install(body.Agent, ""); err != nil {
				c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			message := "适配器安装完成，请重启 Agent"
			if body.Agent == "codebuddy" {
				message = "适配器安装完成，请在 CodeBuddy /hooks 中确认并重启 CLI"
			}
			if body.Agent == "codebuddy-desktop" {
				message = "桌面桥安装完成，请重启 CodeBuddy CN 并打开目标工作区"
			}
			c.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message})
		case r.Method == http.MethodPost && r.URL.Path == "/api/unbind":
			if err := c.unbind(); err != nil {
				c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			c.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		default:
			c.writeJSON(w, http.StatusNotFound, map[string]any{"error": "接口不存在"})
		}
		return
	}
	http.NotFound(w, r)
}

func (c *connector) bindings(w http.ResponseWriter, phoneID string, remove bool) {
	server := strings.TrimSuffix(c.url, "/device")
	server = strings.Replace(server, "wss://", "https://", 1)
	server = strings.Replace(server, "ws://", "http://", 1)
	method := http.MethodGet
	var body io.Reader
	if remove {
		method = http.MethodPost
		value, _ := json.Marshal(map[string]string{"phoneId": phoneID})
		body = strings.NewReader(string(value))
	}
	ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, server+"/device/bindings", body)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("X-AgentTower-Device-Id", c.deviceID)
		req.Header.Set("Content-Type", "application/json")
		var res *http.Response
		res, err = (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err == nil {
			defer res.Body.Close()
			var result map[string]any
			if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&result) == nil {
				c.writeJSON(w, res.StatusCode, result)
				return
			}
			err = fmt.Errorf("服务器返回无效响应")
		}
	}
	c.writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
}

func (c *connector) writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (c *connector) managementStatus() map[string]any {
	c.mu.Lock()
	localPeers := len(c.peers)
	c.mu.Unlock()
	snapshot := c.state.snapshot()
	return map[string]any{
		"deviceId":         c.deviceID,
		"relayUrl":         c.url,
		"localPort":        c.listener.Addr().String(),
		"relayConnected":   c.relayReady.Load(),
		"localPeers":       localPeers,
		"configPath":       c.configPath,
		"disabledProjects": c.disabledProjectList(),
		"state":            snapshot,
		"updatedAt":        time.Now().UnixMilli(),
	}
}

func (c *connector) unbind() error {
	if !c.unbinding.CompareAndSwap(false, true) {
		return http.ErrServerClosed
	}
	defer func() {
		if c.ctx.Err() == nil {
			c.unbinding.Store(false)
		}
	}()
	server := strings.TrimSuffix(strings.TrimSuffix(c.url, "/device"), "/")
	server = strings.Replace(server, "wss://", "https://", 1)
	server = strings.Replace(server, "ws://", "http://", 1)
	// The relay closes the WebSocket as soon as credentials are revoked, which
	// cancels c.ctx. Keep the HTTP response independent of that shutdown.
	if _, err := post(context.Background(), server, "/device/unbind", map[string]any{"deviceId": c.deviceID}, c.token); err != nil {
		return err
	}
	if c.configPath != "" {
		if err := os.Remove(c.configPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		c.cancel()
	}()
	return nil
}

var managementHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>AgentTower Connector</title>
<style>
:root{color-scheme:dark;--bg:#101815;--card:#18231f;--line:#2b3a34;--text:#eef7f2;--muted:#9aaca3;--green:#35d48d;--red:#ff6b72;--amber:#f2b84b;--glow:#1d4032;--group:#20342c;--input:#111b17;--button:#21372e;--button-border:#35604d;--hover:#2d4b3d;--danger:#39242a;--danger-border:#784047;--toast:#244337}*{box-sizing:border-box}body{margin:0;background:radial-gradient(circle at 10% 0,var(--glow) 0,transparent 38%),var(--bg);color:var(--text);font:15px system-ui,-apple-system,Segoe UI,sans-serif}main{max-width:980px;margin:0 auto;padding:38px 20px 60px}header{display:flex;justify-content:space-between;align-items:end;margin-bottom:26px}h1{margin:0 0 8px;font-size:30px}h2{font-size:17px;margin:0 0 16px}.muted{color:var(--muted)}.grid{display:grid;grid-template-columns:repeat(3,1fr);gap:14px;margin-bottom:18px}.card{background:color-mix(in srgb,var(--card) 94%,transparent);border:1px solid var(--line);border-radius:16px;padding:20px;box-shadow:0 12px 30px #0002}.value{font-size:24px;font-weight:700;margin-top:8px}.ok{color:var(--green)}.warn{color:var(--amber)}.bad{color:var(--red)}.wide{margin-bottom:18px}.row{display:flex;justify-content:space-between;align-items:center;gap:15px;padding:11px 0;border-bottom:1px solid var(--line)}.row:last-child{border-bottom:0}.group{border:1px solid var(--line);border-radius:12px;margin:10px 0;overflow:hidden}.group-head{display:flex;justify-content:space-between;padding:12px 14px;background:var(--group)}.group-body{padding:0 14px}.project-tools{display:flex;gap:10px;align-items:center;flex-wrap:wrap;margin:14px 0}.project-search{flex:1;min-width:220px;border:1px solid var(--line);border-radius:10px;background:var(--input);color:var(--text);padding:10px 12px;outline:none}.project-search:focus{border-color:#4c9d77;box-shadow:0 0 0 3px #35d48d20}.agent-head,.project-head{align-items:center;gap:12px}.agent-title,.project-title{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.forwarding{display:flex;align-items:center;gap:9px;color:var(--muted);font-size:13px;white-space:nowrap}.switch{position:relative;width:42px;height:24px;display:inline-block}.switch input{opacity:0;width:0;height:0}.slider{position:absolute;inset:0;border-radius:20px;background:#52635b;cursor:pointer;transition:.18s}.slider:before{content:"";position:absolute;width:18px;height:18px;left:3px;top:3px;border-radius:50%;background:white;transition:.18s}.switch input:checked+.slider{background:var(--green)}.switch input:indeterminate+.slider{background:var(--amber)}.switch input:checked+.slider:before{transform:translateX(18px)}.switch input:indeterminate+.slider:before{transform:translateX(9px)}.switch input:disabled+.slider{cursor:wait;opacity:.6}.actions{display:flex;flex-wrap:wrap;gap:10px}button{border:1px solid var(--button-border);border-radius:10px;background:var(--button);color:var(--text);padding:10px 14px;cursor:pointer}button:hover{background:var(--hover)}button:disabled{cursor:wait;opacity:.55}button.danger{border-color:var(--danger-border);background:var(--danger)}.agents{display:grid;grid-template-columns:repeat(2,1fr);gap:10px}.agent{display:flex;justify-content:space-between;align-items:center;border:1px solid var(--line);border-radius:12px;padding:13px}.empty{color:var(--muted);padding:10px 0}.toast{position:fixed;right:20px;bottom:20px;background:var(--toast);border:1px solid #4c9d77;border-radius:10px;padding:12px 15px;display:none}@media(max-width:680px){.grid,.agents{grid-template-columns:1fr}header{display:block}.actions button{width:100%}.project-tools{align-items:stretch}.project-search{width:100%;flex-basis:100%}.agent-head,.project-head{align-items:flex-start}.forwarding{margin-top:2px}}
:root[data-theme="light"]{color-scheme:light;--bg:#f3f7f5;--card:#fff;--line:#d4e0d9;--text:#182b22;--muted:#586d61;--green:#087b4a;--red:#bf303e;--amber:#946000;--glow:#dcefe3;--group:#edf4ef;--input:#fff;--button:#e4f1e9;--button-border:#a8cbb6;--hover:#d2e9db;--danger:#fff0f1;--danger-border:#dfabb1;--toast:#e4f1e9}.preferences{display:flex;gap:8px;align-items:center;flex-wrap:wrap}.language-picker{display:inline-flex;align-items:center;gap:8px}.language-icon{width:20px;height:20px;flex-shrink:0;color:var(--muted)}select{background:var(--input);color:var(--text);border:1px solid var(--line);border-radius:10px;padding:10px}button:focus-visible,select:focus-visible,.switch input:focus-visible+.slider{outline:2px solid var(--green);outline-offset:3px}.row>span,.row>b{overflow-wrap:anywhere;min-width:0}@media(max-width:680px){.preferences{margin-top:18px}}
</style></head><body><main><header><div><div class="muted">AGENTTOWER CONNECTOR</div><h1 data-i18n="本地管理">本地管理</h1><div class="muted" data-i18n="在本机查看连接状态、安装适配器和管理设备凭证">在本机查看连接状态、安装适配器和管理设备凭证</div></div><div class="preferences"><label class="language-picker"><svg class="language-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><ellipse cx="12" cy="12" rx="4" ry="9"/><path d="M3 12h18"/></svg><select id="language" aria-label="Language" onchange="setLanguage(this.value)"><option value="zh-CN">简体中文</option><option value="en">English</option></select></label><select id="theme" aria-label="Theme" onchange="setTheme(this.value)"><option value="light">Light</option><option value="dark">Dark</option></select><button onclick="refresh()" data-i18n="刷新">刷新</button></div></header>
<section class="grid"><div class="card"><div class="muted" data-i18n="服务器连接">服务器连接</div><div id="relay" class="value" data-i18n="加载中">加载中</div></div><div class="card"><div class="muted" data-i18n="本地 Agent">本地 Agent</div><div id="peers" class="value">—</div></div><div class="card"><div class="muted" data-i18n="待审批">待审批</div><div id="permissions" class="value">—</div></div></section>
<section class="card wide"><h2 data-i18n="Connector 信息">Connector 信息</h2><div class="row"><span class="muted" data-i18n="设备">设备</span><b id="device">—</b></div><div class="row"><span class="muted" data-i18n="中转地址">中转地址</span><span id="url">—</span></div><div class="row"><span class="muted" data-i18n="本地监听">本地监听</span><span id="port">—</span></div><div class="row"><span class="muted" data-i18n="凭证文件">凭证文件</span><span id="config">—</span></div></section>
<section class="card wide"><h2 data-i18n="扫码绑定终端">扫码绑定终端</h2><p class="muted" data-i18n="生成一次性二维码后，用 AgentTower 手机端扫描并确认。二维码 5 分钟内有效，确认后会自动消失。">生成一次性二维码后，用 AgentTower 手机端扫描并确认。二维码 5 分钟内有效，确认后会自动消失。</p><div class="actions"><button onclick="newBinding()" data-i18n="生成绑定二维码">生成绑定二维码</button></div><div id="bindingBox" class="empty" data-i18n="尚未生成绑定二维码">尚未生成绑定二维码</div></section>
<section class="card wide"><h2 data-i18n="安装 Agent 适配器">安装 Agent 适配器</h2><p class="muted" data-i18n="选择你正在使用的 Agent。安装完成后请重启 Agent，使会话状态和审批能力生效。">选择你正在使用的 Agent。安装完成后请重启 Agent，使会话状态和审批能力生效。</p><div class="agents"><div class="agent"><span>OpenCode</span><button onclick="install('opencode')" data-i18n="安装">安装</button></div><div class="agent"><span>Claude Code</span><button onclick="install('claude')" data-i18n="安装">安装</button></div><div class="agent"><span>CodeBuddy Code CLI</span><button onclick="install('codebuddy')" data-i18n="安装">安装</button></div><div class="agent"><span data-i18n="CodeBuddy CN 桌面版">CodeBuddy CN 桌面版</span><button onclick="install('codebuddy-desktop')" data-i18n="安装">安装</button></div><div class="agent"><span>Cursor</span><button onclick="install('cursor')" data-i18n="安装">安装</button></div><div class="agent"><span>Codex</span><button onclick="install('codex')" data-i18n="安装">安装</button></div></div></section>
<section class="card wide"><h2 data-i18n="已绑定终端">已绑定终端</h2><p class="muted" data-i18n="这些手机或控制端当前拥有此 Connector 的访问权限。解除后不会删除设备凭证。">这些手机或控制端当前拥有此 Connector 的访问权限。解除后不会删除设备凭证。</p><div id="bindings" class="empty" data-i18n="加载中">加载中</div></section><section class="card wide"><h2 data-i18n="项目转发">项目转发</h2><p class="muted" data-i18n="关闭长期不用的项目后，它的实例、会话和审批将只保留在本机，不再发送到服务器。设置会保存在本机。每个 Agent 右侧的开关可快速控制其全部项目。">关闭长期不用的项目后，它的实例、会话和审批将只保留在本机，不再发送到服务器。设置会保存在本机。每个 Agent 右侧的开关可快速控制其全部项目。</p><div class="project-tools"><input id="projectSearch" class="project-search" type="search" data-i18n-placeholder="搜索 Agent 或项目路径" placeholder="搜索 Agent 或项目路径" autocomplete="off" oninput="searchProjects()"><span id="projectCount" class="muted">—</span></div><div id="state" class="empty" data-i18n="加载中">加载中</div></section><section class="card wide"><h2 data-i18n="危险操作">危险操作</h2><p class="muted" data-i18n="解绑会撤销此设备与全部手机的关系，并删除本地设备凭证。">解绑会撤销此设备与全部手机的关系，并删除本地设备凭证。</p><button class="danger" onclick="unbind()" data-i18n="解绑此设备">解绑此设备</button></section></main><div id="toast" class="toast"></div>
<script>
const $=id=>document.getElementById(id);
const english={"本地管理": "Local management", "在本机查看连接状态、安装适配器和管理设备凭证": "View connection status, install adapters, and manage device credentials locally", "刷新": "Refresh", "服务器连接": "Server connection", "加载中": "Loading", "本地 Agent": "Local Agents", "待审批": "Pending approvals", "Connector 信息": "Connector information", "设备": "Device", "中转地址": "Relay URL", "本地监听": "Local listener", "凭证文件": "Credential file", "扫码绑定终端": "Pair a terminal", "生成一次性二维码后，用 AgentTower 手机端扫描并确认。二维码 5 分钟内有效，确认后会自动消失。": "Generate a one-time QR code, then scan and confirm in the AgentTower mobile app. The code expires in 5 minutes and disappears after confirmation.", "生成绑定二维码": "Generate pairing QR code", "尚未生成绑定二维码": "No pairing QR code generated", "安装 Agent 适配器": "Install Agent adapters", "选择你正在使用的 Agent。安装完成后请重启 Agent，使会话状态和审批能力生效。": "Choose your Agent. Restart it after installation to enable session updates and approvals.", "安装": "Install", "CodeBuddy CN 桌面版": "CodeBuddy CN Desktop", "已绑定终端": "Paired terminals", "这些手机或控制端当前拥有此 Connector 的访问权限。解除后不会删除设备凭证。": "These phones or consoles can access this Connector. Removing access keeps the device credentials.", "项目转发": "Project forwarding", "关闭长期不用的项目后，它的实例、会话和审批将只保留在本机，不再发送到服务器。设置会保存在本机。每个 Agent 右侧的开关可快速控制其全部项目。": "Disable unused projects to keep their instances, sessions, and approvals locally. Settings are saved on this computer. Use the switch beside an Agent to control all its projects.", "搜索 Agent 或项目路径": "Search Agent or project path", "危险操作": "Danger zone", "解绑会撤销此设备与全部手机的关系，并删除本地设备凭证。": "Unpairing revokes access for all phones and deletes the local device credentials.", "解绑此设备": "Unpair this device", "操作失败": "Operation failed", "其他 Agent": "Other Agent", "没有匹配的项目。": "No matching projects.", "尚未发现本地 Agent，请先安装适配器并重启 Agent。": "No local Agents found. Install an adapter and restart your Agent.", "全部转发": "Forward all", "部分转发": "Partially forwarded", "全部关闭": "All disabled", "未指定项目": "No project specified", "转发": "Forward", "不转发": "Disabled", "在线": "Online", "离线": "Offline", "保存中": "Saving", "已开启项目转发": "Project forwarding enabled", "已停止项目转发": "Project forwarding disabled", "已连接": "Connected", "连接中": "Connecting", "绑定码已过期，请重新生成": "Pairing code expired. Generate a new code.", "终端": "Terminal", "未知平台": "Unknown platform", "解除绑定": "Remove access", "暂无已绑定终端": "No paired terminals", "当前中转服务器尚未支持设备侧绑定管理，请升级服务器后重试。": "This relay server does not support device binding management yet. Upgrade the server and try again.", "已解除绑定": "Access removed", "正在安装…": "Installing…", "确定解绑此设备？这会撤销全部手机访问并删除本地凭证。": "Unpair this device? This revokes access for all phones and deletes local credentials.", "解绑成功，Connector 即将停止": "Device unpaired. Connector will stop shortly", "适配器安装完成，请重启 Agent": "Adapter installed. Restart your Agent.", "适配器安装完成，请在 CodeBuddy /hooks 中确认并重启 CLI": "Adapter installed. Confirm in CodeBuddy /hooks and restart the CLI.", "桌面桥安装完成，请重启 CodeBuddy CN 并打开目标工作区": "Desktop bridge installed. Restart CodeBuddy CN and open the target workspace.", "请求格式无效": "Invalid request", "手机标识无效": "Invalid phone ID", "不支持的 Agent": "Unsupported Agent", "接口不存在": "Endpoint not found", "手机凭证已失效": "Phone credentials expired", "设备凭证已失效": "Device credentials expired", "服务器返回无效响应": "Invalid server response", "绑定码：": "Pairing code: ", "请用手机扫描二维码或手动输入绑定码": "Scan the QR code with your phone or enter the pairing code manually", "设备绑定二维码": "Device pairing QR code", "绑定成功，手机已获得访问权限。": "Paired successfully. Your phone now has access.", "匹配 {count} 个项目": "{count} matching projects", "共 {count} 个项目": "{count} projects", "{count} 个实例 / {projects} 个项目": "{count} instances / {projects} projects", "{count} 个": "{count}", "{count} 项": "{count}", "{count} 秒": "{count} s", "确定关闭 {agent} 下全部 {count} 个项目的转发？": "Disable forwarding for all {count} projects under {agent}?", "已开启 {agent} 的全部项目转发": "Forwarding enabled for all {agent} projects", "已关闭 {agent} 的全部项目转发": "Forwarding disabled for all {agent} projects", "确定解除 {name} 对此设备的访问权限？": "Remove access to this device for {name}?"};
function savedPreference(key){try{return localStorage.getItem(key)}catch{return null}}
function savePreference(key,value){try{localStorage.setItem(key,value)}catch{}}
let language=savedPreference('agenttower.connector.language')|| (navigator.language.startsWith('zh')?'zh-CN':'en');
if(!['zh-CN','en'].includes(language))language='zh-CN';
function t(key,values={}){return (language==='en'?(english[key]||key):key).replace(/\{(\w+)\}/g,(_,name)=>values[name]??'{'+name+'}')}
function applyLanguage(){document.documentElement.lang=language;$('language').value=language;document.querySelectorAll('[data-i18n]').forEach(el=>el.textContent=t(el.dataset.i18n));document.querySelectorAll('[data-i18n-placeholder]').forEach(el=>el.placeholder=t(el.dataset.i18nPlaceholder));document.querySelectorAll('[data-i18n-alt]').forEach(el=>el.alt=t(el.dataset.i18nAlt))}
function setLanguage(value){language=value;savePreference('agenttower.connector.language',value);applyLanguage();stateSignature='';refresh();loadBindings()}
function setTheme(value){document.documentElement.dataset.theme=value;$('theme').value=value;savePreference('agenttower.connector.theme',value)}
applyLanguage();
setTheme(savedPreference('agenttower.connector.theme')==='light'?'light':savedPreference('agenttower.connector.theme')==='dark'?'dark':matchMedia('(prefers-color-scheme: light)').matches?'light':'dark');

function toast(text){$('toast').textContent=text;$('toast').style.display='block';setTimeout(()=>$('toast').style.display='none',3500)}
async function api(path,options){const r=await fetch(path,options);const d=await r.json();if(!r.ok)throw Error(d.error||t('操作失败'));return d}
function escapeHTML(s){return String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
let stateSignature='';
let bindingTimer;
let latestState=null,latestDisabled=[],visibleProjects=[];
function renderState(state,disabledProjects,query=''){
  query=query.trim().toLocaleLowerCase();
  const disabled=new Set((disabledProjects||[]).map(p=>JSON.stringify([p.agent,p.directory||''])));
  const instances=state.instances.filter(i=>!query||String(i.agent||'').toLocaleLowerCase().includes(query)||String(i.directory||'').toLocaleLowerCase().includes(query));
  const signature=JSON.stringify([query,instances.map(i=>[i.id,i.agent,i.directory,i.online]).sort((a,b)=>String(a[0]).localeCompare(String(b[0]))),[...disabled].sort()]);
  if(signature===stateSignature)return;
  stateSignature=signature;
  const groups=new Map();
  const allAgentProjects=new Map();
  for(const instance of state.instances){const agent=instance.agent||t('其他 Agent'),directory=instance.directory||'';if(!allAgentProjects.has(agent))allAgentProjects.set(agent,new Map());allAgentProjects.get(agent).set(directory,{agent,directory})}
  for(const instance of instances){
    const agent=instance.agent||t('其他 Agent'),directory=instance.directory||'';
    if(!groups.has(agent))groups.set(agent,new Map());
    const projects=groups.get(agent);
    if(!projects.has(directory))projects.set(directory,[]);
    projects.get(directory).push(instance);
  }
  visibleProjects=[];
  for(const [agent,projects] of groups)for(const directory of projects.keys())visibleProjects.push({agent,directory});
  $('projectCount').textContent=t(query?'匹配 {count} 个项目':'共 {count} 个项目',{count:visibleProjects.length});
  $('state').replaceChildren();
  if(!groups.size){$('state').textContent=query?t('没有匹配的项目。'):t('尚未发现本地 Agent，请先安装适配器并重启 Agent。');return}
  for(const [agent,projects] of [...groups].sort((a,b)=>a[0].localeCompare(b[0]))){
    const count=[...projects.values()].reduce((n,list)=>n+list.length,0);
    const section=document.createElement('details');section.className='group';
    const heading=document.createElement('summary');heading.className='group-head agent-head';
    const headingTitle=document.createElement('span');headingTitle.className='agent-title';headingTitle.textContent=agent+' · '+t('{count} 个实例 / {projects} 个项目',{count,projects:projects.size});
    const agentProjects=[...allAgentProjects.get(agent).values()],enabledCount=agentProjects.filter(p=>!disabled.has(JSON.stringify([p.agent,p.directory]))).length;
    const headingControl=document.createElement('span');headingControl.className='forwarding';headingControl.onclick=e=>e.stopPropagation();
    const headingLabel=document.createElement('span');const headingSwitch=document.createElement('label');headingSwitch.className='switch';const headingInput=document.createElement('input');headingInput.type='checkbox';headingInput.checked=enabledCount===agentProjects.length;headingInput.indeterminate=enabledCount>0&&enabledCount<agentProjects.length;const headingSlider=document.createElement('span');headingSlider.className='slider';headingLabel.textContent=enabledCount===agentProjects.length?t('全部转发'):enabledCount?t('部分转发'):t('全部关闭');
    headingInput.onchange=()=>bulkAgentForwarding(agent,agentProjects,headingInput,headingLabel);headingSwitch.append(headingInput,headingSlider);headingControl.append(headingLabel,headingSwitch);heading.append(headingTitle,headingControl);section.append(heading);
    const body=document.createElement('div');body.className='group-body';section.append(body);
    for(const [directory,instances] of [...projects].sort((a,b)=>a[0].localeCompare(b[0]))){
      const project=document.createElement('details');project.className='group';
      const summary=document.createElement('summary');summary.className='group-head project-head';
      const title=document.createElement('span');title.className='project-title';title.textContent=(directory||t('未指定项目'))+' · '+instances.filter(i=>i.online).length+'/'+instances.length+' '+t('在线');
      const control=document.createElement('span');control.className='forwarding';control.onclick=e=>e.stopPropagation();
      const label=document.createElement('span');const toggle=document.createElement('label');toggle.className='switch';const input=document.createElement('input');input.type='checkbox';input.checked=!disabled.has(JSON.stringify([agent,directory]));const slider=document.createElement('span');slider.className='slider';label.textContent=input.checked?t('转发'):t('不转发');
      input.onchange=()=>setForwarding(agent,directory,input,label);toggle.append(input,slider);control.append(label,toggle);summary.append(title,control);project.append(summary);
      const list=document.createElement('div');list.className='group-body';project.append(list);
      for(const i of instances){const row=document.createElement('div');row.className='row';const name=document.createElement('span');name.textContent=i.id;const status=document.createElement('span');status.className=i.online?'ok':'muted';status.textContent=i.online?t('在线'):t('离线');row.append(name,status);list.append(row)}
      body.append(project);
    }
    $('state').append(section);
  }
}
function searchProjects(){if(latestState){stateSignature='';renderState(latestState,latestDisabled,$('projectSearch').value)}}
async function setForwarding(agent,directory,input,label){const enabled=input.checked;input.disabled=true;label.textContent=t('保存中');try{await api('/api/forwarding',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({agent,directory,enabled})});toast(enabled?t('已开启项目转发'):t('已停止项目转发'));stateSignature='';await refresh()}catch(e){input.checked=!enabled;label.textContent=input.checked?t('转发'):t('不转发');toast(t(e.message))}finally{input.disabled=false}}
async function bulkAgentForwarding(agent,projects,input,label){const enabled=input.checked;input.indeterminate=false;if(!enabled&&!confirm(t('确定关闭 {agent} 下全部 {count} 个项目的转发？',{agent,count:projects.length}))){stateSignature='';renderState(latestState,latestDisabled,$('projectSearch').value);return}input.disabled=true;label.textContent=t('保存中');try{await api('/api/forwarding/bulk',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({projects,enabled})});toast(t(enabled?'已开启 {agent} 的全部项目转发':'已关闭 {agent} 的全部项目转发',{agent}));stateSignature='';await refresh()}catch(e){toast(t(e.message));stateSignature='';renderState(latestState,latestDisabled,$('projectSearch').value)}finally{input.disabled=false}}
async function refresh(){try{const d=await api('/api/status');$('relay').textContent=d.relayConnected?t('已连接'):t('连接中');$('relay').className='value '+(d.relayConnected?'ok':'warn');$('peers').textContent=t('{count} 个',{count:d.localPeers});$('permissions').textContent=t('{count} 项',{count:d.state.permissions.length});$('device').textContent=d.deviceId;$('url').textContent=d.relayUrl;$('port').textContent=d.localPort;$('config').textContent=d.configPath;latestState=d.state;latestDisabled=d.disabledProjects||[];renderState(latestState,latestDisabled,$('projectSearch').value)}catch(e){toast(t(e.message))}}
async function newBinding(){clearInterval(bindingTimer);try{delete $('bindingBox').dataset.i18n;const d=await api('/api/binding/new',{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'});$('bindingBox').innerHTML='<div class="row"><div><b><span data-i18n="绑定码：">'+escapeHTML(t("绑定码："))+'</span>'+escapeHTML(d.code)+'</b><div class="muted" data-i18n="请用手机扫描二维码或手动输入绑定码">'+escapeHTML(t("请用手机扫描二维码或手动输入绑定码"))+'</div></div><span class="warn" id="bindingCountdown"></span></div><div style="text-align:center;padding:18px"><img data-i18n-alt="设备绑定二维码" alt="'+escapeHTML(t("设备绑定二维码"))+'" style="width:280px;max-width:100%;background:#fff;padding:12px;border-radius:12px" src="data:image/svg+xml;charset=utf-8,'+encodeURIComponent(d.svg)+'"></div>';const expires=d.expiresAt;bindingTimer=setInterval(async()=>{const left=Math.max(0,expires-Date.now());const el=$('bindingCountdown');if(el)el.textContent=t('{count} 秒',{count:Math.ceil(left/1000)});if(!left){clearInterval(bindingTimer);$('bindingBox').dataset.i18n='绑定码已过期，请重新生成';$('bindingBox').textContent=t('绑定码已过期，请重新生成');return}try{const status=await api('/api/binding/status');if(status.status==='approved'){clearInterval(bindingTimer);$('bindingBox').innerHTML='<div class="ok" data-i18n="绑定成功，手机已获得访问权限。">'+escapeHTML(t("绑定成功，手机已获得访问权限。"))+'</div>';loadBindings()}}catch(e){if(e.message.includes('过期')){clearInterval(bindingTimer);$('bindingBox').textContent=e.message}}},2000)}catch(e){toast(t(e.message))}}
async function loadBindings(){try{const d=await api('/api/bindings');$('bindings').replaceChildren();for(const item of d.bindings){const row=document.createElement('div');row.className='row';const info=document.createElement('div');const name=document.createElement('b');name.textContent=item.phoneName||item.phoneId;const details=document.createElement('div');details.className='muted';details.textContent=(item.source||t('终端'))+' · '+(item.phonePlatform||t('未知平台'))+' · '+(item.binding?.boundAt?new Date(item.binding.boundAt).toLocaleString(language):'');info.append(name,details);const button=document.createElement('button');button.className='danger';button.textContent=t('解除绑定');button.onclick=()=>removeBinding(item.phoneId,item.phoneName||item.phoneId);row.append(info,button);$('bindings').append(row)}if(!d.bindings.length)$('bindings').textContent=t('暂无已绑定终端')}catch(e){$('bindings').textContent=['手机凭证已失效','接口不存在','设备凭证已失效'].some(text=>e.message.includes(text))?t('当前中转服务器尚未支持设备侧绑定管理，请升级服务器后重试。'):t(e.message)}}
async function removeBinding(id,name){if(!confirm(t('确定解除 {name} 对此设备的访问权限？',{name})))return;try{await api('/api/bindings/remove',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({phoneId:id})});toast(t('已解除绑定'));await loadBindings()}catch(e){toast(t(e.message))}}
async function install(agent){try{toast(t('正在安装…'));const d=await api('/api/install',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({agent})});toast(t(d.message))}catch(e){toast(t(e.message))}}
async function unbind(){if(!confirm(t('确定解绑此设备？这会撤销全部手机访问并删除本地凭证。')))return;try{await api('/api/unbind',{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'});toast(t('解绑成功，Connector 即将停止'))}catch(e){toast(t(e.message))}}
refresh();loadBindings();setInterval(refresh,5000);setInterval(loadBindings,30000)
</script></body></html>`
