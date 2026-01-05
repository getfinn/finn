// Package devserver - spreadsheet_watcher.go provides file watching for spreadsheet previews
package devserver

import (
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// SpreadsheetWatcher watches spreadsheet files for changes
type SpreadsheetWatcher struct {
	watcher   *fsnotify.Watcher
	files     map[string]string // filePath -> folderID
	callbacks map[string]func() // filePath -> callback
	mu        sync.RWMutex
	done      chan struct{}

	// Debounce to avoid multiple triggers for the same change
	lastChange map[string]time.Time
}

// NewSpreadsheetWatcher creates a new spreadsheet file watcher
func NewSpreadsheetWatcher() (*SpreadsheetWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	sw := &SpreadsheetWatcher{
		watcher:    watcher,
		files:      make(map[string]string),
		callbacks:  make(map[string]func()),
		done:       make(chan struct{}),
		lastChange: make(map[string]time.Time),
	}

	go sw.run()

	return sw, nil
}

// WatchFile starts watching a spreadsheet file for changes
func (sw *SpreadsheetWatcher) WatchFile(filePath, folderID string, onChange func()) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return err
	}

	// Watch the directory containing the file (fsnotify works better with directories)
	dir := filepath.Dir(absPath)

	// Add to watcher if not already watching this directory
	alreadyWatching := false
	for path := range sw.files {
		if filepath.Dir(path) == dir {
			alreadyWatching = true
			break
		}
	}

	if !alreadyWatching {
		if err := sw.watcher.Add(dir); err != nil {
			return err
		}
		log.Printf("👁  Watching directory for spreadsheet changes: %s", dir)
	}

	sw.files[absPath] = folderID
	sw.callbacks[absPath] = onChange

	log.Printf("📊 Watching spreadsheet file: %s", absPath)
	return nil
}

// UnwatchFile stops watching a spreadsheet file
func (sw *SpreadsheetWatcher) UnwatchFile(filePath string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	absPath, _ := filepath.Abs(filePath)

	delete(sw.files, absPath)
	delete(sw.callbacks, absPath)
	delete(sw.lastChange, absPath)

	// Check if we should remove the directory watch
	dir := filepath.Dir(absPath)
	stillWatching := false
	for path := range sw.files {
		if filepath.Dir(path) == dir {
			stillWatching = true
			break
		}
	}

	if !stillWatching {
		sw.watcher.Remove(dir)
		log.Printf("👁  Stopped watching directory: %s", dir)
	}

	log.Printf("📊 Stopped watching spreadsheet file: %s", absPath)
}

// UnwatchFolder stops watching all files for a folder
func (sw *SpreadsheetWatcher) UnwatchFolder(folderID string) {
	sw.mu.Lock()
	filesToRemove := []string{}
	for path, fid := range sw.files {
		if fid == folderID {
			filesToRemove = append(filesToRemove, path)
		}
	}
	sw.mu.Unlock()

	for _, path := range filesToRemove {
		sw.UnwatchFile(path)
	}
}

// run is the main event loop
func (sw *SpreadsheetWatcher) run() {
	for {
		select {
		case <-sw.done:
			return

		case event, ok := <-sw.watcher.Events:
			if !ok {
				return
			}

			// We only care about write events
			if event.Op&fsnotify.Write != fsnotify.Write {
				continue
			}

			sw.handleFileChange(event.Name)

		case err, ok := <-sw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("⚠️  Spreadsheet watcher error: %v", err)
		}
	}
}

// handleFileChange processes a file change event
func (sw *SpreadsheetWatcher) handleFileChange(filePath string) {
	absPath, _ := filepath.Abs(filePath)

	sw.mu.RLock()
	callback, exists := sw.callbacks[absPath]
	lastChange := sw.lastChange[absPath]
	sw.mu.RUnlock()

	if !exists {
		return
	}

	// Debounce: ignore changes within 500ms of the last one
	now := time.Now()
	if now.Sub(lastChange) < 500*time.Millisecond {
		return
	}

	sw.mu.Lock()
	sw.lastChange[absPath] = now
	sw.mu.Unlock()

	log.Printf("📊 Spreadsheet file changed: %s", filepath.Base(absPath))

	if callback != nil {
		callback()
	}
}

// Close stops the watcher and cleans up
func (sw *SpreadsheetWatcher) Close() error {
	close(sw.done)
	return sw.watcher.Close()
}

// IsSpreadsheetFile checks if a file is a spreadsheet file
func IsSpreadsheetFile(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return ext == ".xlsx" || ext == ".xls" || ext == ".csv" || ext == ".ods"
}

// GetSpreadsheetFiles returns all spreadsheet files in a directory
func GetSpreadsheetFiles(dirPath string) ([]string, error) {
	var files []string

	entries, err := filepath.Glob(filepath.Join(dirPath, "*"))
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if IsSpreadsheetFile(entry) {
			files = append(files, entry)
		}
	}

	return files, nil
}
