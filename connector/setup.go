package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

const defaultServer = "https://relay.example.com"

func openManagement(address string) {
	if os.Getenv("AGENTTOWER_NO_BROWSER") == "1" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	case "darwin":
		cmd = exec.Command("open", address)
	default:
		cmd = exec.Command("xdg-open", address)
	}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

func setupDevice(path, name, id string) error {
	port := 17654
	if v := os.Getenv("AGENTTOWER_LOCAL_PORT"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &port); err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("本机端口无效")
		}
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer listener.Close()
	return serveSetup(listener, path, name, id)
}

func serveSetup(listener net.Listener, path, name, id string) error {
	var mu sync.Mutex
	config, _ := identity(defaultServer, name, id, path)
	c := &connector{ctx: context.Background(), configPath: path, deviceID: config.DeviceID, url: config.RelayURL}
	done := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != listener.Addr().String() && r.Host != strings.Replace(listener.Addr().String(), "127.0.0.1", "localhost", 1) {
			http.Error(w, "invalid host", 403)
			return
		}
		if r.Method == "POST" {
			origin := r.Header.Get("Origin")
			u, e := url.Parse(origin)
			if e != nil || (origin != "" && (u.Scheme != "http" || u.Host != r.Host)) || r.Header.Get("Content-Type") != "application/json" {
				http.Error(w, "invalid origin or content type", 403)
				return
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "GET" && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, setupHTML)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/setup":
			json.NewEncoder(w).Encode(map[string]string{"server": config.Server, "name": config.Name})
		case r.Method == "POST" && r.URL.Path == "/api/setup":
			var body struct{ Server, Name string }
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil {
				c.writeJSON(w, 400, map[string]string{"error": "请求格式无效"})
				return
			}
			server, e := endpoint(strings.TrimSpace(body.Server))
			if e != nil {
				c.writeJSON(w, 400, map[string]string{"error": e.Error()})
				return
			}
			config, _ = identity(server, strings.TrimSpace(body.Name), id, path)
			c.url, c.deviceID = config.RelayURL, config.DeviceID
			c.newBinding(w)
		case r.Method == "GET" && r.URL.Path == "/api/setup/status":
			if c.binding == nil {
				c.writeJSON(w, 400, map[string]string{"error": "请先生成绑定二维码"})
				return
			}
			result, e := post(r.Context(), config.Server, "/device-bind/status", map[string]string{"bindId": c.binding.id, "pollSecret": c.binding.secret}, "")
			if e != nil {
				c.writeJSON(w, 502, map[string]string{"error": e.Error()})
				return
			}
			if str(result["status"]) == "approved" {
				config.Token = str(result["token"])
				if config.Token == "" || str(result["deviceId"]) != config.DeviceID {
					c.writeJSON(w, 502, map[string]string{"error": "服务器返回的设备凭证不匹配"})
					return
				}
				if e = saveConfig(path, config); e != nil {
					c.writeJSON(w, 500, map[string]string{"error": e.Error()})
					return
				}
				c.writeJSON(w, 200, map[string]string{"status": "approved"})
				select {
				case done <- struct{}{}:
				default:
				}
				return
			}
			c.writeJSON(w, 200, result)
		default:
			http.NotFound(w, r)
		}
	})
	server := &http.Server{Handler: handler}
	failures := make(chan error, 1)
	go func() { failures <- server.Serve(listener) }()
	address := "http://" + listener.Addr().String() + "/"
	fmt.Printf("[AgentTower] 首次配置：%s\n", address)
	openManagement(address)
	select {
	case <-done:
		return server.Shutdown(context.Background())
	case err := <-failures:
		return err
	}
}

const setupHTML = `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>AgentTower 首次配置</title><style>body{font:16px system-ui;max-width:600px;margin:60px auto;padding:24px;background:#f5f7fb;color:#182238}label{display:block;margin:20px 0}input,button{font:inherit;padding:12px;box-sizing:border-box;width:100%}button{cursor:pointer}#qr svg{width:240px}#error{color:#b42318}</style><h1>欢迎使用 AgentTower</h1><p>确认设备名称，生成二维码后用手机端扫描绑定。</p><form id="form"><label>服务器地址<input id="server" type="url" required></label><label>设备名称<input id="name" required maxlength="128"></label><button>生成绑定二维码</button></form><p id="error" role="alert"></p><div id="qr"></div><p id="status"></p><script>
const form=document.querySelector('#form'),error=document.querySelector('#error');let timer;
async function api(path,body){const r=await fetch(path,body?{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}:{});const v=await r.json();if(!r.ok)throw Error(v.error||'操作失败');return v}
api('/api/setup').then(v=>{document.querySelector('#server').value=v.server;document.querySelector('#name').value=v.name}).catch(e=>error.textContent=e.message);
form.onsubmit=async e=>{e.preventDefault();clearTimeout(timer);error.textContent='';form.querySelector('button').disabled=true;try{const v=await api('/api/setup',{server:document.querySelector('#server').value,name:document.querySelector('#name').value});document.querySelector('#qr').innerHTML=v.svg;document.querySelector('#status').textContent='绑定码：'+v.code+'（5 分钟内有效）';const poll=async()=>{try{const s=await api('/api/setup/status');if(s.status==='approved'){document.querySelector('#status').textContent='绑定成功，正在打开管理页面…';setTimeout(()=>location.reload(),1500);return}if(Date.now()>=v.expiresAt)throw Error('绑定码已过期，请重新生成');timer=setTimeout(poll,2000)}catch(e){error.textContent=e.message;form.querySelector('button').disabled=false}};timer=setTimeout(poll,2000)}catch(e){error.textContent=e.message;form.querySelector('button').disabled=false}};
</script></html>`
