// Package devserver - static_watcher.go provides file watching for static HTML/CSS/JS previews
package devserver

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// StaticWatcher watches static files (HTML, CSS, JS) for changes recursively
type StaticWatcher struct {
	watcher   *fsnotify.Watcher
	roots     map[string]string // rootPath -> folderID (the top-level watched folders)
	callbacks map[string]func() // folderID -> callback
	mu        sync.RWMutex
	done      chan struct{}

	// Debounce to avoid multiple triggers for rapid changes
	lastChange map[string]time.Time // folderID -> last change time
}

// NewStaticWatcher creates a new static file watcher
func NewStaticWatcher() (*StaticWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	sw := &StaticWatcher{
		watcher:    watcher,
		roots:      make(map[string]string),
		callbacks:  make(map[string]func()),
		done:       make(chan struct{}),
		lastChange: make(map[string]time.Time),
	}

	go sw.run()

	return sw, nil
}

// WatchFolder starts watching a folder and all its subdirectories for static file changes
func (sw *StaticWatcher) WatchFolder(folderPath, folderID string, onChange func()) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	absPath, err := filepath.Abs(folderPath)
	if err != nil {
		return err
	}

	// Check if already watching this folder
	if _, exists := sw.roots[absPath]; exists {
		// Update the callback
		sw.callbacks[folderID] = onChange
		return nil
	}

	// Walk the directory tree and add all directories to the watcher
	// This enables recursive watching since fsnotify doesn't do it natively
	dirCount := 0
	err = filepath.WalkDir(absPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Skip directories we can't access
		}
		if !d.IsDir() {
			return nil
		}
		// Skip common non-essential directories
		name := d.Name()
		if name == "node_modules" || name == ".git" || name == ".next" ||
		   name == ".nuxt" || name == "dist" || name == "build" ||
		   name == "__pycache__" || name == ".venv" || name == "venv" ||
		   name == "vendor" || name == "target" || name == ".idea" {
			return filepath.SkipDir
		}
		if err := sw.watcher.Add(path); err != nil {
			log.Printf("⚠️  Failed to watch directory %s: %v", path, err)
			return nil // Continue with other directories
		}
		dirCount++
		return nil
	})
	if err != nil {
		return err
	}

	sw.roots[absPath] = folderID
	sw.callbacks[folderID] = onChange

	log.Printf("👁  Watching folder for static file changes: %s (%d directories)", absPath, dirCount)
	return nil
}

// UnwatchFolder stops watching a folder and all its subdirectories
func (sw *StaticWatcher) UnwatchFolder(folderID string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	// Find the root folder for this folderID
	var rootToRemove string
	for path, fid := range sw.roots {
		if fid == folderID {
			rootToRemove = path
			break
		}
	}

	if rootToRemove == "" {
		return
	}

	// Remove all watched paths under this root
	// We need to walk again because we might have added subdirectories
	filepath.WalkDir(rootToRemove, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		sw.watcher.Remove(path)
		return nil
	})

	delete(sw.roots, rootToRemove)
	delete(sw.callbacks, folderID)
	delete(sw.lastChange, folderID)
	log.Printf("👁  Stopped watching folder: %s", rootToRemove)
}

// run is the main event loop
func (sw *StaticWatcher) run() {
	for {
		select {
		case <-sw.done:
			return

		case event, ok := <-sw.watcher.Events:
			if !ok {
				return
			}

			// Handle new directories being created - add them to the watcher
			if event.Op&fsnotify.Create != 0 {
				sw.handleCreate(event.Name)
			}

			// We care about write and create events for files
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}

			// Check if this is a static file we care about
			if !IsStaticFile(event.Name) {
				continue
			}

			sw.handleFileChange(event.Name)

		case err, ok := <-sw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("⚠️  Static watcher error: %v", err)
		}
	}
}

// handleCreate handles newly created files/directories
func (sw *StaticWatcher) handleCreate(path string) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return
	}

	// Check if it's actually a directory using os.Stat
	fileInfo, err := os.Stat(absPath)
	if err != nil || !fileInfo.IsDir() {
		return // Not a directory or can't stat it
	}

	// Skip common non-essential directories
	name := filepath.Base(absPath)
	if name == "node_modules" || name == ".git" || name == ".next" ||
	   name == ".nuxt" || name == "dist" || name == "build" ||
	   name == "__pycache__" || name == ".venv" || name == "venv" ||
	   name == "vendor" || name == "target" || name == ".idea" {
		return
	}

	sw.mu.RLock()
	// Find which root this belongs to
	for root := range sw.roots {
		if strings.HasPrefix(absPath, root+string(filepath.Separator)) {
			sw.mu.RUnlock()
			if err := sw.watcher.Add(absPath); err == nil {
				log.Printf("👁  Added new directory to watch: %s", name)
			}
			return
		}
	}
	sw.mu.RUnlock()
}

// handleFileChange processes a file change event
func (sw *StaticWatcher) handleFileChange(filePath string) {
	absPath, _ := filepath.Abs(filePath)

	sw.mu.RLock()
	// Find which root folder this file belongs to
	var folderID string
	for root, fid := range sw.roots {
		if strings.HasPrefix(absPath, root+string(filepath.Separator)) || filepath.Dir(absPath) == root {
			folderID = fid
			break
		}
	}
	if folderID == "" {
		sw.mu.RUnlock()
		return
	}
	callback := sw.callbacks[folderID]
	lastChange := sw.lastChange[folderID]
	sw.mu.RUnlock()

	// Debounce: ignore changes within 300ms of the last one
	// Using shorter debounce than spreadsheet (300ms vs 500ms) for snappier reload
	now := time.Now()
	if now.Sub(lastChange) < 300*time.Millisecond {
		return
	}

	sw.mu.Lock()
	sw.lastChange[folderID] = now
	sw.mu.Unlock()

	log.Printf("📄 Static file changed: %s", filepath.Base(absPath))

	if callback != nil {
		callback()
	}
}

// Close stops the watcher and cleans up
func (sw *StaticWatcher) Close() error {
	close(sw.done)
	return sw.watcher.Close()
}

// IsStaticFile checks if a file is a static web file (HTML, CSS, JS)
func IsStaticFile(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".html", ".htm", ".css", ".js", ".json", ".svg", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico":
		return true
	default:
		return false
	}
}
