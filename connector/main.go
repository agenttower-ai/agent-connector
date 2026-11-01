package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed adapters/opencode.mjs adapters/deepseek-harness.mjs adapters/codebuddy-desktop.js adapters/codebuddy-desktop.json
var assets embed.FS

type deviceConfig struct {
	Version          int          `json:"version"`
	Server           string       `json:"server"`
	RelayURL         string       `json:"relayUrl"`
	DeviceID         string       `json:"deviceId"`
	Name             string       `json:"name"`
	Token            string       `json:"token"`
	DisabledProjects []projectRef `json:"disabledProjects,omitempty"`
}

type projectRef struct {
	Agent     string `json:"agent"`
	Directory string `json:"directory"`
}

func option(f *flag.FlagSet, name, def, usage string) *string { return f.String(name, def, usage) }
func configFile(v string) string {
	if v != "" {
		return v
	}
	if e := os.Getenv("AGENTTOWER_DEVICE_CONFIG"); e != "" {
		return e
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agenttower", "device.json")
}
func readConfig(path string) (deviceConfig, error) {
	var c deviceConfig
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	e = json.Unmarshal(b, &c)
	return c, e
}
func saveConfig(path string, c deviceConfig) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(path, b, 0600)
}
func endpoint(raw string) (string, error) {
	if strings.HasPrefix(raw, "[http") {
		return "", errors.New("服务器地址不能使用 Markdown 链接格式，请直接填写 https://tower.example.com")
	}
	u, e := url.Parse(raw)
	if e != nil || u == nil || u.Host == "" {
		return "", errors.New("请使用 --server 指定有效的 AgentTower HTTPS 地址")
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if !(u.Scheme == "https" || (local && u.Scheme == "http")) || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("远程服务器必须使用 HTTPS，且地址不能包含路径或凭证")
	}
	return u.Scheme + "://" + u.Host, nil
}
func post(ctx context.Context, server, route string, body any, token string) (map[string]any, error) {
	b, _ := json.Marshal(body)
	req, e := http.NewRequestWithContext(ctx, "POST", server+route, strings.NewReader(string(b)))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.Client{Timeout: 15 * time.Second}
	res, e := client.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	data, e := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if e != nil {
		return nil, e
	}
	var out map[string]any
	if e = json.Unmarshal(data, &out); e != nil {
		return nil, e
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("%v", out["error"])
	}
	return out, nil
}
func platform() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s %s %s", runtime.GOOS, host, runtime.GOARCH)
}
func identity(server, name, id, file string) (deviceConfig, string) {
	old, _ := readConfig(file)
	if old.Server != server {
		old = deviceConfig{}
	}
	if name == "" {
		name = old.Name
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	if id == "" {
		id = old.DeviceID
	}
	if id == "" {
		v := strings.ToLower(name)
		b := strings.Builder{}
		for _, r := range v {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				b.WriteRune(r)
			} else {
				b.WriteByte('-')
			}
		}
		slug := strings.Trim(b.String(), "-")
		if len(slug) > 48 {
			slug = slug[:48]
		}
		if slug == "" {
			slug = "device"
		}
		id = slug + "-" + runtime.GOOS
	}
	if old.DeviceID != id {
		old.Token = ""
	}
	return deviceConfig{Version: 1, Server: server, RelayURL: strings.Replace(server, "http", "ws", 1) + "/device", DeviceID: id, Name: name, Token: old.Token, DisabledProjects: old.DisabledProjects}, old.Token
}
func autoBind(ctx context.Context, server, ticket, name, id, file string) error {
	if len(ticket) != 64 {
		return errors.New("一次性安装凭证无效，请在手机重新复制安装指令")
	}
	if _, e := hex.DecodeString(ticket); e != nil {
		return errors.New("一次性安装凭证无效")
	}
	c, old := identity(server, name, id, file)
	body := map[string]any{"setupTicket": ticket, "deviceId": c.DeviceID, "name": c.Name, "source": "connector-cli", "platform": platform()}
	if old != "" {
		body["deviceToken"] = old
	}
	r, e := post(ctx, server, "/device-bind/auto", body, "")
	if e != nil {
		return e
	}
	c.DeviceID = str(r["deviceId"])
	c.Name = str(r["name"])
	c.Token = str(r["token"])
	if c.Token == "" {
		c.Token = old
	}
	if c.Token == "" {
		return errors.New("服务器未返回设备凭证")
	}
	if e = saveConfig(file, c); e != nil {
		return e
	}
	fmt.Printf("设备“%s”已自动关联到发起安装的手机。\n", c.Name)
	return nil
}
func bind(ctx context.Context, server, name, id, file, qrFile string) error {
	c, old := identity(server, name, id, file)
	body := map[string]any{"deviceId": c.DeviceID, "name": c.Name, "source": "connector-cli", "platform": platform()}
	if old != "" {
		body["deviceToken"] = old
	}
	r, e := post(ctx, server, "/device-bind/request", body, "")
	if e != nil {
		return e
	}
	code := str(r["code"])
	fmt.Printf("\n设备绑定码：%s\n请在手机中打开“添加设备”，输入此代码。\n", code)
	if qrFile == "" {
		qrFile = filepath.Join(filepath.Dir(file), "binding-qr.svg")
	}
	qrAbsolute, _ := filepath.Abs(qrFile)
	configAbsolute, _ := filepath.Abs(file)
	if qrAbsolute == configAbsolute {
		return errors.New("二维码路径不能与设备凭证路径相同")
	}
	if qrFile != file {
		if png, e := qrcode.New("agenttower://bind?code="+code, qrcode.Medium); e == nil {
			if svg := qrSVG(png.Bitmap()); os.WriteFile(qrFile, []byte(svg), 0600) == nil {
				fmt.Print(qrTerminal(png.Bitmap()))
				fmt.Printf("绑定二维码：%s\n", qrFile)
			}
		}
	}
	defer os.Remove(qrFile)
	expiry := time.UnixMilli(int64(number(r["expiresAt"])))
	if expiry.Before(time.Now()) {
		expiry = time.Now().Add(5 * time.Minute)
	}
	for time.Now().Before(expiry) {
		status, e := post(ctx, server, "/device-bind/status", map[string]any{"bindId": r["bindId"], "pollSecret": r["pollSecret"]}, "")
		if e != nil {
			return e
		}
		if str(status["status"]) == "approved" {
			c.DeviceID = str(status["deviceId"])
			c.Name = str(status["name"])
			c.Token = str(status["token"])
			if c.Token == "" {
				c.Token = old
			}
			if c.Token == "" {
				return errors.New("服务器未返回设备凭证")
			}
			if e = saveConfig(file, c); e != nil {
				return e
			}
			fmt.Printf("设备“%s”绑定成功，凭证已保存到 %s\n", c.Name, file)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return errors.New("设备绑定码已过期，请重新执行 agenttower bind")
}
func number(v any) float64 { n, _ := v.(float64); return n }
func qrSVG(bits [][]bool) string {
	const unit = 5
	margin := 4
	size := (len(bits) + margin*2) * unit
	var b strings.Builder
	fmt.Fprintf(&b, "<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 %d %d\"><rect width=\"100%%\" height=\"100%%\" fill=\"white\"/>", size, size)
	for y, row := range bits {
		for x, on := range row {
			if on {
				fmt.Fprintf(&b, "<rect x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\" fill=\"black\"/>", (x+margin)*unit, (y+margin)*unit, unit, unit)
			}
		}
	}
	b.WriteString("</svg>")
	return b.String()
}
func qrTerminal(bits [][]bool) string {
	var b strings.Builder
	for _, row := range bits {
		for _, on := range row {
			if on {
				b.WriteString("██")
			} else {
				b.WriteString("  ")
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
func run(args []string) error {
	launch := len(args) == 0
	if len(args) == 0 {
		args = []string{"start"}
	}
	cmd := args[0]
	if cmd == "claude-hook" {
		return claudeHook()
	}
	if cmd == "codebuddy-hook" {
		return codebuddyHook()
	}
	if cmd == "cursor-hook" {
		return cursorHook()
	}
	if cmd == "codex-hook" {
		return codexHook()
	}
	if cmd == "start" && len(args) > 1 && args[1] == "bind" {
		return errors.New("start 和 bind 是两个独立命令")
	}
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	server := option(f, "server", "", "")
	name := option(f, "name", "", "")
	id := option(f, "device-id", "", "")
	file := option(f, "config", "", "")
	ticket := option(f, "setup-ticket", "", "")
	qr := option(f, "qr-file", "", "")
	target := option(f, "target", "", "")
	f.Bool("approval", false, "") // Accepted for compatibility; Cursor approval is now the default.
	if cmd == "install" {
		if len(args) < 2 {
			return errors.New("安装适配器：agenttower install opencode|opencode-cli|claude|codebuddy|codebuddy-cli|codebuddy-desktop|cursor|cursor-cli|codex|codex-cli|dsh")
		}
		if e := f.Parse(args[2:]); e != nil {
			return e
		}
		return install(args[1], *target)
	}
	if e := f.Parse(args[1:]); e != nil {
		return e
	}
	path := configFile(*file)
	ctx := context.Background()
	switch cmd {
	case "bind", "start":
		if (cmd == "bind" || *ticket != "") && *server == "" {
			*server = defaultServer
		}
		if *ticket != "" || *server != "" {
			origin, e := endpoint(*server)
			if e != nil {
				return e
			}
			if *ticket != "" {
				if e = autoBind(ctx, origin, *ticket, *name, *id, path); e != nil {
					return e
				}
			} else if cmd == "bind" || !exists(path) {
				if e = bind(ctx, origin, *name, *id, path, *qr); e != nil {
					return e
				}
			}
		} else if cmd == "bind" {
			return errors.New("请使用 --server 指定有效的 AgentTower HTTPS 地址")
		}
		if cmd == "bind" {
			return nil
		}
		c, e := readConfig(path)
		if e != nil && (os.Getenv("AGENT_LIGHT_RELAY_URL") == "" || os.Getenv("AGENT_LIGHT_DEVICE_TOKEN") == "" || os.Getenv("AGENT_LIGHT_DEVICE_ID") == "") {
			if !os.IsNotExist(e) {
				return fmt.Errorf("读取设备配置失败：%w", e)
			}
			if e = setupDevice(path, *name, *id); e != nil {
				return e
			}
			c, e = readConfig(path)
			if e != nil {
				return e
			}
		}
		if v := os.Getenv("AGENT_LIGHT_RELAY_URL"); v != "" {
			c.RelayURL = v
		}
		if v := os.Getenv("AGENT_LIGHT_DEVICE_TOKEN"); v != "" {
			c.Token = v
		}
		if v := os.Getenv("AGENT_LIGHT_DEVICE_ID"); v != "" {
			c.DeviceID = v
		}
		port := 17654
		if v := os.Getenv("AGENTTOWER_LOCAL_PORT"); v != "" {
			if _, e = fmt.Sscanf(v, "%d", &port); e != nil || port < 1 || port > 65535 {
				return errors.New("本机端口无效")
			}
		}
		conn, e := startConnector(c.RelayURL, c.Token, c.DeviceID, port, path)
		if e != nil {
			return e
		}
		defer conn.close()
		fmt.Printf("[AgentTower] Connector 已启动：%s，本机管理 http://127.0.0.1:%d/\n", c.DeviceID, port)
		if launch {
			openManagement(fmt.Sprintf("http://127.0.0.1:%d/", port))
		}
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		select {
		case <-stop:
		case <-conn.ctx.Done():
		}
		return nil
	case "unbind":
		c, e := readConfig(path)
		if e != nil {
			return errors.New("未找到本地设备凭证，无需解绑")
		}
		origin, e := endpoint(c.Server)
		if e != nil {
			return e
		}
		if _, e = post(ctx, origin, "/device/unbind", map[string]any{"deviceId": c.DeviceID}, c.Token); e != nil {
			return e
		}
		if e = os.Remove(path); e != nil {
			return e
		}
		fmt.Printf("设备“%s”已解绑全部手机，本地凭证已删除。\n", c.Name)
		return nil
	default:
		usage()
		return fmt.Errorf("未知命令：%s", cmd)
	}
}
func exists(path string) bool { _, e := os.Stat(path); return e == nil }
func usage() {
	fmt.Println("用法：agenttower bind|start|unbind [--server 地址] [--config 文件路径]\n安装适配器：agenttower install opencode|opencode-cli|claude|codebuddy|codebuddy-cli|codebuddy-desktop|cursor|cursor-cli|codex|codex-cli|dsh [--target 文件路径]")
}
func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "[AgentTower]", e)
		os.Exit(1)
	}
}
func randomID() string { b := make([]byte, 16); rand.Read(b); return hex.EncodeToString(b) }
