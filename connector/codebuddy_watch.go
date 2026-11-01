package main

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch directories rather than individual files: CodeBuddy can replace JSON
// files using a temporary file followed by rename. Only history trees and their
// discovery parents are watched, not checkpoints, caches or generated files.
func codebuddyWatchDirs(root, workspaceRoot string) map[string]bool {
	dirs := map[string]bool{}
	add := func(path string) {
		for {
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				dirs[path] = true
				return
			}
			parent := filepath.Dir(path)
			if parent == path {
				return
			}
			path = parent
		}
	}
	add(root)
	add(workspaceRoot)
	accounts, _ := os.ReadDir(root)
	for _, account := range accounts {
		if !account.IsDir() || account.Name() == "Public" {
			continue
		}
		base := filepath.Join(root, account.Name())
		add(base)
		ide := filepath.Join(base, "CodeBuddyIDE")
		add(ide)
		profiles, _ := os.ReadDir(ide)
		for _, profile := range profiles {
			if !profile.IsDir() {
				continue
			}
			add(filepath.Join(ide, profile.Name()))
			history := filepath.Join(ide, profile.Name(), "history")
			_ = filepath.WalkDir(history, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if entry.IsDir() {
					if entry.Name() == "backups" {
						return filepath.SkipDir
					}
					dirs[path] = true
				}
				return nil
			})
		}
	}
	return dirs
}

func codebuddyWatchRelevant(event fsnotify.Event) bool {
	if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
		return false
	}
	// Directory events discover new accounts/projects/sessions, including when
	// the complete directory was populated before its watch could be installed.
	if event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
		return true
	}
	return strings.EqualFold(filepath.Ext(event.Name), ".json")
}

func watchCodebuddyHistory(ctx context.Context, root, workspaceRoot string, refresh func()) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[CodeBuddy] 文件监听不可用，使用 60 秒校验：%v", err)
		refresh()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}
	defer watcher.Close()
	watched := map[string]bool{}
	reconcile := func() {
		desired := codebuddyWatchDirs(root, workspaceRoot)
		// fsnotify removes watches automatically after watched directories vanish.
		actual := map[string]bool{}
		for _, path := range watcher.WatchList() {
			actual[path] = true
		}
		for path := range watched {
			if !desired[path] {
				_ = watcher.Remove(path)
			}
		}
		watched = map[string]bool{}
		for path := range desired {
			if actual[path] {
				watched[path] = true
				continue
			}
			if err := watcher.Add(path); err == nil {
				watched[path] = true
			} else if !errors.Is(err, fs.ErrNotExist) {
				log.Printf("[CodeBuddy] 无法监听 %s：%v", path, err)
			}
		}
	}
	reconcile()
	refresh()
	check := time.NewTicker(time.Minute)
	defer check.Stop()
	var timer *time.Timer
	var pending <-chan time.Time
	schedule := func() {
		// Fixed coalescing window instead of trailing debounce: sustained streaming
		// writes must not postpone delivery indefinitely.
		if pending == nil {
			timer = time.NewTimer(time.Second)
			pending = timer.C
		}
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if codebuddyWatchRelevant(event) {
				schedule()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[CodeBuddy] 文件监听需要重新同步：%v", err)
			schedule()
		case <-pending:
			pending = nil
			reconcile()
			refresh()
		case <-check.C:
			reconcile()
			refresh()
		}
	}
}
