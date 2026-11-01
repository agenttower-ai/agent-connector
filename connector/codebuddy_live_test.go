package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCodebuddyDesktopLiveConversation(t *testing.T) {
	if os.Getenv("AGENTTOWER_CODEBUDDY_LIVE_TEST") != "1" {
		t.Skip("explicit live IDE test only")
	}
	events := make(chan map[string]any, 256)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var m map[string]any
			if conn.ReadJSON(&m) != nil {
				return
			}
			select {
			case events <- m:
			default:
			}
		}
	}))
	defer sink.Close()
	c, err := startConnector("ws://127.0.0.1:1", "live-test", "live-test", 17654, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(sink.URL, "http", "ws", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.remote = &peer{conn: conn}
	c.mu.Unlock()
	session := "0339789967ee48738d5fbd5c453a0907"
	deadline := time.Now().Add(30 * time.Second)
	for {
		c.mu.Lock()
		connected := c.controls["codebuddy-bridge:b3ba9c9a7a81910eca1d32f6f5a35f2d"] != nil
		c.mu.Unlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("IDE bridge did not connect")
		}
		time.Sleep(time.Second)
	}
	c.handleSession(map[string]any{"type": "control.session", "instanceId": "codebuddy-desktop:" + session, "sessionID": session, "contextId": session, "action": "prompt", "actionID": "agenttower-live-" + randomID(), "text": "AgentTower 连接测试：请只回复 AGENTTOWER_OK，不执行工具、不修改文件。"})
	accepted := false
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	for {
		select {
		case m := <-events:
			event := obj(m["event"])
			if event["type"] == "monitor.action.result" && event["action"] == "prompt" {
				if event["ok"] != true {
					t.Fatalf("IDE submission failed: %v", event["error"])
				}
				accepted = true
				t.Log("IDE accepted submission")
			}
			if accepted && event["type"] == "monitor.output" && event["sessionID"] == session && strings.Contains(str(event["text"]), "AGENTTOWER_OK") {
				t.Log("Received real IDE assistant reply through Connector output event")
				return
			}
		case <-timer.C:
			t.Fatalf("No confirmed assistant reply; submitted=%v", accepted)
		}
	}
}
