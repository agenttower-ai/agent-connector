package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexDesktopProjectsUsesOrderAndAppendsUnordered(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := os.MkdirAll(first, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "state.json")
	data, err := json.Marshal(codexDesktopState{LocalProjects: map[string]codexDesktopProject{
		"a": {ID: "a", Name: "First", RootPaths: []string{first}},
		"b": {ID: "b", Name: "Second", RootPaths: []string{second}},
	}, ProjectOrder: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	projects, err := codexDesktopProjectsFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 || projects[0].ID != "a" || projects[1].ID != "b" {
		t.Fatalf("unexpected project order: %#v", projects)
	}
	if got := codexDesktopProjectDirectory(projects[0]); got != first {
		t.Fatalf("unexpected project directory: %s", got)
	}
}

func TestCodexDesktopRefreshRemovesDeletedProjects(t *testing.T) {
	s := newState()
	s.replaceCodexDesktop(map[string]map[string]any{"codex-desktop:old": {"id": "codex-desktop:old"}}, map[string]map[string]any{
		recordKey("codex-desktop:old", "old"): {"id": "old", "instanceId": "codex-desktop:old"},
	})
	s.replaceCodexDesktop(map[string]map[string]any{}, map[string]map[string]any{})
	if len(s.snapshot()["instances"].([]any)) != 0 || len(s.snapshot()["sessions"].([]any)) != 0 {
		t.Fatal("removed project remains visible")
	}
}

func TestCodexDesktopSkipsUnavailableRoots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	data, err := json.Marshal(codexDesktopState{LocalProjects: map[string]codexDesktopProject{
		"missing": {ID: "missing", Name: "Missing", RootPaths: []string{filepath.Join(t.TempDir(), "gone")}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	projects, err := codexDesktopProjectsFrom(path)
	if err != nil || len(projects) != 0 {
		t.Fatalf("unexpected projects: %#v, %v", projects, err)
	}
}

func TestCodexDesktopProjectInstanceDeclaresNoControl(t *testing.T) {
	instance, session := codexDesktopInstance(codexDesktopProject{ID: "id", Name: "Demo"}, "host", 1)
	if instance["agent"] != "codex" || instance["source"] != "desktop" {
		t.Fatalf("unexpected instance: %#v", instance)
	}
	if obj(instance["capabilities"])["sessionControl"] == true {
		t.Fatal("Codex desktop must not claim session control")
	}
	if session["title"] != "Demo" {
		t.Fatalf("unexpected session: %#v", session)
	}
}
