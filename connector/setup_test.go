package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFirstRunSetup(t *testing.T) {
	t.Setenv("AGENTTOWER_NO_BROWSER", "1")
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/device-bind/request" {
			json.NewEncoder(w).Encode(map[string]any{"code": "ABCDEFGH", "bindId": "bind", "pollSecret": "secret", "expiresAt": time.Now().Add(time.Minute).UnixMilli()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "approved", "deviceId": "test-device", "token": "test-token"})
	}))
	defer relay.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	file := filepath.Join(t.TempDir(), "device.json")
	done := make(chan error, 1)
	go func() { done <- serveSetup(listener, file, "", "test-device") }()
	base := "http://" + listener.Addr().String()
	get := func(route string) map[string]any {
		t.Helper()
		res, e := http.Get(base + route)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		var v map[string]any
		if e = json.NewDecoder(res.Body).Decode(&v); e != nil {
			t.Fatal(e)
		}
		if res.StatusCode != 200 {
			t.Fatal(v)
		}
		return v
	}
	defaults := get("/api/setup")
	if defaults["server"] != defaultServer || defaults["name"] == "" {
		t.Fatal(defaults)
	}
	body, _ := json.Marshal(map[string]string{"server": relay.URL, "name": "My computer"})
	req, _ := http.NewRequest("POST", base+"/api/setup", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.StatusCode)
	}
	res, err = http.Post(base+"/api/setup", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	if get("/api/setup/status")["status"] != "approved" {
		t.Fatal("not approved")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("setup did not close")
	}
	config, err := readConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if config.Server != relay.URL || config.Name != "My computer" || config.Token != "test-token" {
		t.Fatal("configuration not saved")
	}
}
