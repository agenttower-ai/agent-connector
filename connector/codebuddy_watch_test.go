package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodebuddyWatcherHandlesAtomicReplacementAndNewDirectories(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "account", "CodeBuddyIDE", "profile", "history")
	if err := os.MkdirAll(history, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan struct{}, 32)
	done := make(chan struct{})
	go func() { defer close(done); watchCodebuddyHistory(ctx, root, root, func() { updates <- struct{}{} }) }()
	wait := func() {
		t.Helper()
		select {
		case <-updates:
		case <-time.After(5 * time.Second):
			t.Fatal("file update was not detected")
		}
	}
	wait() // startup baseline, with watches already installed
	temp := filepath.Join(history, "index.tmp")
	if err := os.WriteFile(temp, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, filepath.Join(history, "index.json")); err != nil {
		t.Fatal(err)
	}
	wait()
	session := filepath.Join(history, "session", "messages")
	if err := os.MkdirAll(session, 0700); err != nil {
		t.Fatal(err)
	}
	wait()
	if err := os.WriteFile(filepath.Join(session, "message.json"), []byte(`{"text":"first"}`), 0600); err != nil {
		t.Fatal(err)
	}
	wait()
	if err := os.WriteFile(filepath.Join(session, "message.json"), []byte(`{"text":"updated"}`), 0600); err != nil {
		t.Fatal(err)
	}
	wait()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop")
	}
}

func TestCodebuddyBridgeUpdatesSendCapabilityWithoutHistoryScan(t *testing.T) {
	s := newState()
	desktopID := "codebuddy-desktop:session"
	s.instances[desktopID] = map[string]any{"id": desktopID, "directory": "project", "capabilities": map[string]any{"sessionControl": false}}
	s.observe(map[string]any{"type": "monitor.hello", "instanceId": "bridge", "instance": map[string]any{"id": "bridge", "agent": "codebuddy-bridge", "directory": "project"}})
	if obj(s.instances[desktopID]["capabilities"])["sessionControl"] != true {
		t.Fatal("handshake did not enable sending")
	}
	s.disconnect("bridge")
	if obj(s.instances[desktopID]["capabilities"])["sessionControl"] != false {
		t.Fatal("disconnect did not disable sending")
	}
}
