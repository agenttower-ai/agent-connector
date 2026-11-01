package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

type localBinding struct {
	id, secret, code, svg string
	expiresAt             int64
}

func (c *connector) bindingServer() (string, error) {
	u, err := url.Parse(c.url)
	if err != nil || u.Host == "" || u.Path != "/device" {
		return "", errors.New("设备中转地址无效")
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	default:
		return "", errors.New("设备中转地址无效")
	}
	u.Path = ""
	u.RawPath = ""
	return u.String(), nil
}

func (c *connector) newBinding(w http.ResponseWriter) {
	c.bindingMu.Lock()
	defer c.bindingMu.Unlock()
	server, err := c.bindingServer()
	if err != nil {
		c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	name := c.deviceID
	if config, readErr := readConfig(c.configPath); readErr == nil && config.Name != "" {
		name = config.Name
	}
	ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
	defer cancel()
	result, err := post(ctx, server, "/device-bind/request", map[string]any{
		"deviceId": c.deviceID, "name": name, "deviceToken": c.token,
		"source": "connector-local", "platform": platform(),
	}, "")
	if err != nil {
		c.writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	code, id, secret := str(result["code"]), str(result["bindId"]), str(result["pollSecret"])
	if len(code) != 8 || id == "" || secret == "" {
		c.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "服务器返回无效绑定信息"})
		return
	}
	qr, err := qrcode.New("agenttower://bind?code="+code, qrcode.Medium)
	if err != nil {
		c.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "二维码生成失败"})
		return
	}
	expiresAt := int64(number(result["expiresAt"]))
	if expiresAt <= time.Now().UnixMilli() {
		c.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "服务器返回无效过期时间"})
		return
	}
	c.binding = &localBinding{id: id, secret: secret, code: code, svg: qrSVG(qr.Bitmap()), expiresAt: expiresAt}
	c.writeJSON(w, http.StatusOK, map[string]any{"code": code, "svg": c.binding.svg, "expiresAt": expiresAt})
}

func (c *connector) bindingStatus(w http.ResponseWriter) {
	c.bindingMu.Lock()
	defer c.bindingMu.Unlock()
	if c.binding == nil {
		c.writeJSON(w, http.StatusNotFound, map[string]any{"error": "请先生成绑定二维码"})
		return
	}
	if c.binding.expiresAt <= time.Now().UnixMilli() {
		c.binding = nil
		c.writeJSON(w, http.StatusGone, map[string]any{"error": "绑定码已过期，请重新生成"})
		return
	}
	server, err := c.bindingServer()
	if err != nil {
		c.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
	defer cancel()
	status, err := post(ctx, server, "/device-bind/status", map[string]any{"bindId": c.binding.id, "pollSecret": c.binding.secret}, "")
	if err != nil {
		c.writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if str(status["status"]) == "pending" {
		c.writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "expiresAt": c.binding.expiresAt})
		return
	}
	if str(status["status"]) != "approved" || str(status["deviceId"]) != c.deviceID || (str(status["token"]) != "" && str(status["token"]) != c.token) {
		c.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "服务器返回的设备凭证不匹配"})
		return
	}
	c.binding = nil
	c.writeJSON(w, http.StatusOK, map[string]any{"status": "approved"})
}
