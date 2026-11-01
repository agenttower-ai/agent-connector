package main

import (
	"os"
	"testing"
)

func TestCursorDesktopBridgeLiveList(t *testing.T) {
	if os.Getenv("AGENTTOWER_CURSOR_LIVE_TEST") != "1" {
		t.Skip("set AGENTTOWER_CURSOR_LIVE_TEST=1 to check the local Cursor bridge")
	}
	threads, err := cursorDesktopList()
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) == 0 {
		t.Fatal("bridge returned no desktop threads")
	}
	for _, thread := range threads {
		if thread.ID == "" || thread.Title == "" {
			t.Fatalf("incomplete desktop thread: %#v", thread)
		}
	}
}
