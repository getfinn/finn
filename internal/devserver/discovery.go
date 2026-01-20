// Package devserver provides project discovery for monorepo and multi-project support.
//
// This file implements automatic detection of projects within a folder tree,
// supporting multiple programming ecosystems (Node.js, Go, Python, Ruby, etc.).
// The detection is based on marker files (package.json, go.mod, Gemfile, etc.)
// which is the industry-standard approach used by tools like VS Code, Vercel,
// and Heroku.
package devserver

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ecosystem represents the programming language/runtime ecosystem.
type Ecosystem string

const (
	EcosystemNode    Ecosystem = "node"
	EcosystemGo      Ecosystem = "go"
	EcosystemPython  Ecosystem = "python"
	EcosystemRuby    Ecosystem = "ruby"
	EcosystemRust    Ecosystem = "rust"
	EcosystemJava    Ecosystem = "java"
	EcosystemPHP     Ecosystem = "php"
	EcosystemDotNet  Ecosystem = "dotnet"
	EcosystemElixir  Ecosystem = "elixir"
	EcosystemUnknown Ecosystem = "unknown"
)

// DiscoveredProject represents a project found during discovery.
type DiscoveredProject struct {
	Path        string    `json:"path"`
	RelPath     string    `json:"rel_path"`
	Name        string    `json:"name"`
	Ecosystem   Ecosystem `json:"ecosystem"`
	Framework   string    `json:"framework"`
	HasDevCmd   bool      `json:"has_dev_cmd"`
	DevCommand  string    `json:"dev_command,omitempty"`
	MarkerFile  string    `json:"marker_file"`
}

// FileActivity represents a single file edit event.
type FileActivity struct {
	FilePath  string    `json:"file_path"`
	FolderID  string    `json:"folder_id"`
	Timestamp time.Time `json:"timestamp"`
}

// ActivityTracker tracks recent file edits to help auto-select projects.
// Thread-safe for concurrent access.
type ActivityTracker struct {
	mu           sync.RWMutex
	activities   map[string][]FileActivity // folderID -> activities
	maxAge       time.Duration
	maxPerFolder int
	lastCleanup  time.Time
	cleanupInterval time.Duration
}

// NewActivityTracker creates an ActivityTracker with default settings.
func NewActivityTracker() *ActivityTracker {
	return &ActivityTracker{
		activities:      make(map[string][]FileActivity),
		maxAge:          30 * time.Minute,
		maxPerFolder:    100,
		cleanupInterval: 5 * time.Minute,
	}
}

// RecordActivity logs a file edit for activity tracking.
func (a *ActivityTracker) RecordActivity(folderID, filePath string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()

	// Add new activity
	a.activities[folderID] = append(a.activities[folderID], FileActivity{
		FilePath:  filePath,
		FolderID:  folderID,
		Timestamp: now,
	})

	// Trim old activities for this folder
	a.trimActivities(folderID, now)

	// Periodically clean up stale folders to prevent memory growth
	if now.Sub(a.lastCleanup) > a.cleanupInterval {
		a.cleanupStaleFolders(now)
		a.lastCleanup = now
	}
}

// cleanupStaleFolders removes folder entries that have no recent activity.
// Must be called with lock held.
func (a *ActivityTracker) cleanupStaleFolders(now time.Time) {
	cutoff := now.Add(-a.maxAge)
	for folderID, activities := range a.activities {
		// Check if folder has any recent activity
		hasRecent := false
		for _, act := range activities {
			if act.Timestamp.After(cutoff) {
				hasRecent = true
				break
			}
		}
		// Remove folder entirely if all activities are stale
		if !hasRecent {
			delete(a.activities, folderID)
		}
	}
}

// GetRecentFiles returns the most recently edited files for a folder.
func (a *ActivityTracker) GetRecentFiles(folderID string, limit int) []string {
	a.mu.RLock()
	defer a.mu.RUnlock()

	activities := a.activities[folderID]
	if len(activities) == 0 {
		return nil
	}

	// Return most recent first
	result := make([]string, 0, min(limit, len(activities)))
	for i := len(activities) - 1; i >= 0 && len(result) < limit; i-- {
		// Skip stale entries
		if time.Since(activities[i].Timestamp) > a.maxAge {
			continue
		}
		result = append(result, activities[i].FilePath)
	}
	return result
}

// GetMostActiveProject finds the project with the most recent activity.
// Returns nil if no activity found for the given folder.
func (a *ActivityTracker) GetMostActiveProject(folderID, folderPath string, projects []DiscoveredProject) *DiscoveredProject {
	recentFiles := a.GetRecentFiles(folderID, 20)
	if len(recentFiles) == 0 {
		return nil
	}

	// Count activity per project
	projectActivity := make(map[string]int)
	for _, filePath := range recentFiles {
		for _, p := range projects {
			if strings.HasPrefix(filePath, p.Path+string(filepath.Separator)) || filePath == p.Path {
				projectActivity[p.Path]++
			}
		}
	}

	// Find project with most activity
	var bestProject *DiscoveredProject
	var maxActivity int
	for i := range projects {
		activity := projectActivity[projects[i].Path]
		if activity > maxActivity {
			maxActivity = activity
			bestProject = &projects[i]
		}
	}

	return bestProject
}

// trimActivities removes old entries and enforces limits.
func (a *ActivityTracker) trimActivities(folderID string, now time.Time) {
	activities := a.activities[folderID]

	// Remove stale entries
	cutoff := now.Add(-a.maxAge)
	filtered := activities[:0]
	for _, act := range activities {
		if act.Timestamp.After(cutoff) {
			filtered = append(filtered, act)
		}
	}

	// Enforce max limit
	if len(filtered) > a.maxPerFolder {
		filtered = filtered[len(filtered)-a.maxPerFolder:]
	}

	a.activities[folderID] = filtered
}

// Clear removes all activity data for a folder.
func (a *ActivityTracker) Clear(folderID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.activities, folderID)
}

// PreviewConfig stores user-configured preview settings for a project.
type PreviewConfig struct {
	DevCommand  string    `json:"dev_command"`
	ProjectPath string    `json:"project_path"` // Relative to folder root
	UpdatedAt   time.Time `json:"updated_at"`
}

// PreviewConfigCache caches preview configurations per folder.
// Thread-safe for concurrent access.
type PreviewConfigCache struct {
	mu      sync.RWMutex
	configs map[string]PreviewConfig // folderID -> config
}

// NewPreviewConfigCache creates an empty config cache.
func NewPreviewConfigCache() *PreviewConfigCache {
	return &PreviewConfigCache{
		configs: make(map[string]PreviewConfig),
	}
}

// Get retrieves the cached config for a folder.
func (c *PreviewConfigCache) Get(folderID string) (PreviewConfig, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cfg, ok := c.configs[folderID]
	return cfg, ok
}

// Set stores a preview config for a folder.
func (c *PreviewConfigCache) Set(folderID string, cfg PreviewConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg.UpdatedAt = time.Now()
	c.configs[folderID] = cfg
}

// Clear removes the cached config for a folder.
func (c *PreviewConfigCache) Clear(folderID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.configs, folderID)
}

// markerConfig defines how to identify a project ecosystem.
type markerConfig struct {
	filename  string
	ecosystem Ecosystem
	isGlob    bool
}

// markers defines the files that identify each ecosystem.
// Order matters - more specific markers should come first.
var markers = []markerConfig{
	// Node.js
	{"package.json", EcosystemNode, false},
	// Go
	{"go.mod", EcosystemGo, false},
	// Python
	{"pyproject.toml", EcosystemPython, false},
	{"manage.py", EcosystemPython, false},
	{"requirements.txt", EcosystemPython, false},
	{"Pipfile", EcosystemPython, false},
	{"setup.py", EcosystemPython, false},
	// Ruby
	{"Gemfile", EcosystemRuby, false},
	// Rust
	{"Cargo.toml", EcosystemRust, false},
	// Java
	{"pom.xml", EcosystemJava, false},
	{"build.gradle", EcosystemJava, false},
	{"build.gradle.kts", EcosystemJava, false},
	// PHP
	{"composer.json", EcosystemPHP, false},
	// .NET
	{"*.csproj", EcosystemDotNet, true},
	{"*.fsproj", EcosystemDotNet, true},
	// Elixir
	{"mix.exs", EcosystemElixir, false},
}

// skipDirs contains directories to skip during traversal.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"vendor":       true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	"target":       true,
	"build":        true,
	"dist":         true,
	".next":        true,
	".nuxt":        true,
	"coverage":     true,
	".idea":        true,
	".vscode":      true,
}

// ============================================================================
// Framework Registry - Data-driven framework detection
// ============================================================================
//
// To add a new framework:
// 1. Add an entry to the appropriate ecosystem's framework list below
// 2. That's it! The detection and dev command will work automatically.
//
// Each framework rule specifies:
// - Name: The framework identifier (e.g., "nextjs", "rails")
// - DevCommand: The command to run the dev server
// - Detect: A function that returns true if this framework is detected
//
// Rules are checked in order - put more specific frameworks first.

// frameworkRule defines how to detect a framework and run its dev server.
type frameworkRule struct {
	Name       string
	DevCommand string
	Detect     func(projectPath string, deps map[string]bool, fileContent string) bool
}

// nodeFrameworks defines detection rules for Node.js frameworks.
// Order matters - more specific frameworks should come first.
var nodeFrameworks = []frameworkRule{
	{"nextjs", "npm run dev", func(_ string, deps map[string]bool, _ string) bool { return deps["next"] }},
	{"nuxt", "npm run dev", func(_ string, deps map[string]bool, _ string) bool { return deps["nuxt"] }},
	{"sveltekit", "npm run dev", func(_ string, deps map[string]bool, _ string) bool { return deps["@sveltejs/kit"] }},
	{"remix", "npm run dev", func(_ string, deps map[string]bool, _ string) bool { return deps["@remix-run/react"] || deps["remix"] }},
	{"astro", "npm run dev", func(_ string, deps map[string]bool, _ string) bool { return deps["astro"] }},
	{"gatsby", "npm run develop", func(_ string, deps map[string]bool, _ string) bool { return deps["gatsby"] }},
	{"vite", "npm run dev", func(_ string, deps map[string]bool, _ string) bool { return deps["vite"] }},
	{"cra", "npm start", func(_ string, deps map[string]bool, _ string) bool { return deps["react-scripts"] }},
	{"angular", "npm start", func(_ string, deps map[string]bool, _ string) bool { return deps["@angular/core"] }},
	{"express", "npm run dev", func(_ string, deps map[string]bool, _ string) bool { return deps["express"] }},
}

// pythonFrameworks defines detection rules for Python frameworks.
var pythonFrameworks = []frameworkRule{
	{"django", "python manage.py runserver", func(path string, _ map[string]bool, _ string) bool {
		return fileExists(filepath.Join(path, "manage.py"))
	}},
	{"fastapi", "uvicorn main:app --reload", func(_ string, _ map[string]bool, content string) bool {
		return strings.Contains(strings.ToLower(content), "fastapi")
	}},
	{"flask", "flask run", func(_ string, _ map[string]bool, content string) bool {
		return strings.Contains(strings.ToLower(content), "flask")
	}},
	{"streamlit", "streamlit run app.py", func(_ string, _ map[string]bool, content string) bool {
		return strings.Contains(strings.ToLower(content), "streamlit")
	}},
}

// rubyFrameworks defines detection rules for Ruby frameworks.
var rubyFrameworks = []frameworkRule{
	{"rails", "rails server", func(_ string, _ map[string]bool, content string) bool {
		return containsGem(content, "rails")
	}},
	{"sinatra", "ruby app.rb", func(_ string, _ map[string]bool, content string) bool {
		return containsGem(content, "sinatra")
	}},
	{"hanami", "hanami server", func(_ string, _ map[string]bool, content string) bool {
		return containsGem(content, "hanami")
	}},
}

// phpFrameworks defines detection rules for PHP frameworks.
var phpFrameworks = []frameworkRule{
	{"laravel", "php artisan serve", func(path string, _ map[string]bool, _ string) bool {
		return fileExists(filepath.Join(path, "artisan"))
	}},
	{"symfony", "symfony server:start", func(path string, _ map[string]bool, _ string) bool {
		return fileExists(filepath.Join(path, "symfony.lock"))
	}},
}

// javaFrameworks defines detection rules for Java frameworks.
var javaFrameworks = []frameworkRule{
	{"spring-boot-maven", "mvn spring-boot:run", func(path string, _ map[string]bool, _ string) bool {
		if data, err := os.ReadFile(filepath.Join(path, "pom.xml")); err == nil {
			return strings.Contains(string(data), "spring-boot")
		}
		return false
	}},
	{"spring-boot-gradle", "./gradlew bootRun", func(path string, _ map[string]bool, _ string) bool {
		for _, f := range []string{"build.gradle", "build.gradle.kts"} {
			if data, err := os.ReadFile(filepath.Join(path, f)); err == nil {
				if strings.Contains(string(data), "spring-boot") {
					return true
				}
			}
		}
		return false
	}},
}

// rustFrameworks defines detection rules for Rust frameworks.
var rustFrameworks = []frameworkRule{
	{"actix-web", "cargo run", func(_ string, _ map[string]bool, content string) bool {
		return strings.Contains(content, "actix-web")
	}},
	{"axum", "cargo run", func(_ string, _ map[string]bool, content string) bool {
		return strings.Contains(content, "axum")
	}},
	{"rocket", "cargo run", func(_ string, _ map[string]bool, content string) bool {
		return strings.Contains(content, "rocket")
	}},
}

// elixirFrameworks defines detection rules for Elixir frameworks.
var elixirFrameworks = []frameworkRule{
	{"phoenix", "mix phx.server", func(_ string, _ map[string]bool, content string) bool {
		return strings.Contains(content, ":phoenix")
	}},
}

// detectFramework checks a list of framework rules and returns the first match.
func detectFramework(rules []frameworkRule, projectPath string, deps map[string]bool, fileContent string) *frameworkRule {
	for i := range rules {
		if rules[i].Detect(projectPath, deps, fileContent) {
			return &rules[i]
		}
	}
	return nil
}

// DiscoverProjects finds all projects within a folder tree up to maxDepth levels.
func DiscoverProjects(rootPath string, maxDepth int) ([]DiscoveredProject, error) {
	rootPath = filepath.Clean(rootPath)
	var projects []DiscoveredProject
	seen := make(map[string]bool)

	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if !d.IsDir() {
			return nil
		}

		// Skip excluded directories
		if skipDirs[d.Name()] {
			return filepath.SkipDir
		}

		// Check depth
		relPath, _ := filepath.Rel(rootPath, path)
		depth := strings.Count(relPath, string(filepath.Separator))
		if relPath != "." {
			depth++
		}
		if depth > maxDepth {
			return filepath.SkipDir
		}

		// Check for project markers
		if project := detectProject(path, rootPath); project != nil {
			if !seen[path] {
				seen[path] = true
				projects = append(projects, *project)
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Sort: shallower paths first, then alphabetically
	sort.Slice(projects, func(i, j int) bool {
		di := strings.Count(projects[i].RelPath, string(filepath.Separator))
		dj := strings.Count(projects[j].RelPath, string(filepath.Separator))
		if di != dj {
			return di < dj
		}
		return projects[i].RelPath < projects[j].RelPath
	})

	return projects, nil
}

// detectProject checks if a directory contains a project marker.
func detectProject(dir, root string) *DiscoveredProject {
	for _, m := range markers {
		var found bool
		var markerFile string

		if m.isGlob {
			matches, _ := filepath.Glob(filepath.Join(dir, m.filename))
			if len(matches) > 0 {
				found = true
				markerFile = filepath.Base(matches[0])
			}
		} else {
			if _, err := os.Stat(filepath.Join(dir, m.filename)); err == nil {
				found = true
				markerFile = m.filename
			}
		}

		if found {
			relPath, _ := filepath.Rel(root, dir)
			if relPath == "." {
				relPath = ""
			}

			p := &DiscoveredProject{
				Path:       dir,
				RelPath:    relPath,
				Name:       filepath.Base(dir),
				Ecosystem:  m.ecosystem,
				MarkerFile: markerFile,
			}

			enrichProject(p)
			return p
		}
	}
	return nil
}

// enrichProject adds framework-specific details to a discovered project.
func enrichProject(p *DiscoveredProject) {
	switch p.Ecosystem {
	case EcosystemNode:
		enrichNodeProject(p)
	case EcosystemGo:
		enrichGoProject(p)
	case EcosystemPython:
		enrichPythonProject(p)
	case EcosystemRuby:
		enrichRubyProject(p)
	case EcosystemRust:
		enrichRustProject(p)
	case EcosystemPHP:
		enrichPHPProject(p)
	case EcosystemJava:
		enrichJavaProject(p)
	case EcosystemElixir:
		enrichElixirProject(p)
	}
}

// enrichNodeProject detects Node.js frameworks and dev commands.
func enrichNodeProject(p *DiscoveredProject) {
	data, err := os.ReadFile(filepath.Join(p.Path, "package.json"))
	if err != nil {
		return
	}

	var pkg struct {
		Name    string            `json:"name"`
		Scripts map[string]string `json:"scripts"`
		Deps    map[string]string `json:"dependencies"`
		DevDeps map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return
	}

	if pkg.Name != "" {
		p.Name = pkg.Name
	}

	// Combine dependencies for framework detection
	deps := make(map[string]bool)
	for k := range pkg.Deps {
		deps[k] = true
	}
	for k := range pkg.DevDeps {
		deps[k] = true
	}

	// Use registry to detect framework
	if fw := detectFramework(nodeFrameworks, p.Path, deps, ""); fw != nil {
		p.Framework = fw.Name
		p.HasDevCmd = true
		p.DevCommand = fw.DevCommand
	} else {
		// Fallback: generic Node.js with script detection
		p.Framework = "node"
		if _, ok := pkg.Scripts["dev"]; ok {
			p.HasDevCmd = true
			p.DevCommand = "npm run dev"
		} else if _, ok := pkg.Scripts["start"]; ok {
			p.HasDevCmd = true
			p.DevCommand = "npm start"
		}
	}
}

// enrichGoProject detects Go project details.
func enrichGoProject(p *DiscoveredProject) {
	p.Framework = "go"

	// Read module name
	if data, err := os.ReadFile(filepath.Join(p.Path, "go.mod")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "module ") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					modPath := parts[1]
					p.Name = filepath.Base(modPath)
				}
				break
			}
		}
	}

	// Check for main.go or cmd directory
	if fileExists(filepath.Join(p.Path, "main.go")) {
		p.HasDevCmd = true
		p.DevCommand = "go run ."
	} else if dirExists(filepath.Join(p.Path, "cmd")) {
		entries, _ := os.ReadDir(filepath.Join(p.Path, "cmd"))
		if len(entries) == 1 && entries[0].IsDir() {
			p.HasDevCmd = true
			p.DevCommand = "go run ./cmd/" + entries[0].Name()
		}
	}
}

// enrichPythonProject detects Python frameworks.
func enrichPythonProject(p *DiscoveredProject) {
	p.Framework = "python"

	// Read requirements.txt for framework detection
	var content string
	if data, err := os.ReadFile(filepath.Join(p.Path, "requirements.txt")); err == nil {
		content = string(data)
	}

	// Use registry to detect framework
	if fw := detectFramework(pythonFrameworks, p.Path, nil, content); fw != nil {
		p.Framework = fw.Name
		p.HasDevCmd = true
		p.DevCommand = fw.DevCommand
	}
}

// enrichRubyProject detects Ruby frameworks.
func enrichRubyProject(p *DiscoveredProject) {
	p.Framework = "ruby"

	data, err := os.ReadFile(filepath.Join(p.Path, "Gemfile"))
	if err != nil {
		return
	}

	// Use registry to detect framework
	if fw := detectFramework(rubyFrameworks, p.Path, nil, string(data)); fw != nil {
		p.Framework = fw.Name
		p.HasDevCmd = true
		p.DevCommand = fw.DevCommand
	}
}

// enrichRustProject detects Rust project details.
func enrichRustProject(p *DiscoveredProject) {
	p.Framework = "rust"

	data, err := os.ReadFile(filepath.Join(p.Path, "Cargo.toml"))
	if err != nil {
		return
	}

	content := string(data)

	// Extract package name from Cargo.toml
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name") && strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				p.Name = strings.Trim(strings.TrimSpace(parts[1]), "\"'")
				break
			}
		}
	}

	// Use registry to detect framework
	if fw := detectFramework(rustFrameworks, p.Path, nil, content); fw != nil {
		p.Framework = fw.Name
		p.HasDevCmd = true
		p.DevCommand = fw.DevCommand
	}
}

// enrichPHPProject detects PHP frameworks.
func enrichPHPProject(p *DiscoveredProject) {
	p.Framework = "php"

	// Use registry to detect framework (file-based detection)
	if fw := detectFramework(phpFrameworks, p.Path, nil, ""); fw != nil {
		p.Framework = fw.Name
		p.HasDevCmd = true
		p.DevCommand = fw.DevCommand
	}
}

// enrichJavaProject detects Java frameworks.
func enrichJavaProject(p *DiscoveredProject) {
	p.Framework = "java"

	// Use registry to detect framework (checks pom.xml and build.gradle internally)
	if fw := detectFramework(javaFrameworks, p.Path, nil, ""); fw != nil {
		p.Framework = fw.Name
		p.HasDevCmd = true
		p.DevCommand = fw.DevCommand
	}
}

// enrichElixirProject detects Elixir frameworks.
func enrichElixirProject(p *DiscoveredProject) {
	p.Framework = "elixir"

	// Read mix.exs for framework detection
	var content string
	if data, err := os.ReadFile(filepath.Join(p.Path, "mix.exs")); err == nil {
		content = string(data)
	}

	// Use registry to detect framework
	if fw := detectFramework(elixirFrameworks, p.Path, nil, content); fw != nil {
		p.Framework = fw.Name
		p.HasDevCmd = true
		p.DevCommand = fw.DevCommand
	}
}

// FindProjectRoot walks up from startPath to find the nearest project root.
func FindProjectRoot(startPath string) (string, Ecosystem) {
	current := filepath.Clean(startPath)

	for {
		for _, m := range markers {
			var found bool
			if m.isGlob {
				matches, _ := filepath.Glob(filepath.Join(current, m.filename))
				found = len(matches) > 0
			} else {
				_, err := os.Stat(filepath.Join(current, m.filename))
				found = err == nil
			}
			if found {
				return current, m.ecosystem
			}
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", EcosystemUnknown
		}
		current = parent
	}
}

// PreviewSelection contains the result of project selection for preview.
type PreviewSelection struct {
	AllProjects      []DiscoveredProject `json:"all_projects"`
	PreviewableCount int                 `json:"previewable_count"`
	Selected         *DiscoveredProject  `json:"selected,omitempty"`
	SelectionReason  string              `json:"selection_reason"`
	NeedsUserInput   bool                `json:"needs_user_input"`
}

// SelectProjectForPreview finds the best project for preview within a folder.
// Selection priority:
//  1. Cached config (user previously selected this project)
//  2. Activity context (files recently edited by Claude)
//  3. Context path (specific path provided by caller)
//  4. Single previewable project (no ambiguity)
//  5. Multiple projects (requires user selection)
func SelectProjectForPreview(
	rootPath string,
	folderID string,
	contextPath string,
	activityTracker *ActivityTracker,
	configCache *PreviewConfigCache,
) PreviewSelection {
	result := PreviewSelection{
		SelectionReason: "none",
		NeedsUserInput:  true,
	}

	// Discover projects
	projects, err := DiscoverProjects(rootPath, 3)
	if err != nil {
		log.Printf("Project discovery error: %v", err)
		return result
	}
	result.AllProjects = projects

	// Filter to previewable projects
	var previewable []DiscoveredProject
	for _, p := range projects {
		if p.HasDevCmd {
			previewable = append(previewable, p)
		}
	}
	result.PreviewableCount = len(previewable)

	if len(previewable) == 0 {
		result.SelectionReason = "no_previewable_projects"
		return result
	}

	// If only one previewable project, auto-select it (no ambiguity)
	if len(previewable) == 1 {
		project := previewable[0]
		result.Selected = &project
		result.SelectionReason = "single_project"
		result.NeedsUserInput = false
		return result
	}

	// Multiple previewable projects (monorepo) - always show picker
	// Don't use cache or activity for monorepos - let user choose each time
	// This ensures users are always aware of which project they're previewing
	result.SelectionReason = "multiple_projects"
	result.NeedsUserInput = true
	return result
}

// InstallInfo contains information about how to install dependencies for a project.
type InstallInfo struct {
	Command     string // e.g., "npm install", "bundle install"
	PackageManager string // e.g., "npm", "yarn", "bundler"
	LockFile    string // e.g., "package-lock.json", "yarn.lock"
	DepsDir     string // e.g., "node_modules", "vendor"
}

// GetInstallCommand detects the correct install command for a project based on ecosystem and lock files.
func GetInstallCommand(projectPath string, ecosystem Ecosystem) InstallInfo {
	switch ecosystem {
	case EcosystemNode:
		return detectNodeInstallCommand(projectPath)
	case EcosystemPython:
		return detectPythonInstallCommand(projectPath)
	case EcosystemRuby:
		return detectRubyInstallCommand(projectPath)
	case EcosystemGo:
		return InstallInfo{
			Command:        "go mod download",
			PackageManager: "go",
			LockFile:       "go.sum",
			DepsDir:        "", // Go uses module cache
		}
	case EcosystemRust:
		return InstallInfo{
			Command:        "cargo build",
			PackageManager: "cargo",
			LockFile:       "Cargo.lock",
			DepsDir:        "target",
		}
	case EcosystemJava:
		return detectJavaInstallCommand(projectPath)
	case EcosystemPHP:
		return InstallInfo{
			Command:        "composer install",
			PackageManager: "composer",
			LockFile:       "composer.lock",
			DepsDir:        "vendor",
		}
	case EcosystemDotNet:
		return InstallInfo{
			Command:        "dotnet restore",
			PackageManager: "dotnet",
			LockFile:       "packages.lock.json",
			DepsDir:        "", // Uses global cache
		}
	case EcosystemElixir:
		return InstallInfo{
			Command:        "mix deps.get",
			PackageManager: "mix",
			LockFile:       "mix.lock",
			DepsDir:        "deps",
		}
	default:
		return InstallInfo{}
	}
}

func detectNodeInstallCommand(projectPath string) InstallInfo {
	// Check for lock files in priority order
	if fileExists(filepath.Join(projectPath, "bun.lockb")) {
		return InstallInfo{
			Command:        "bun install",
			PackageManager: "bun",
			LockFile:       "bun.lockb",
			DepsDir:        "node_modules",
		}
	}
	if fileExists(filepath.Join(projectPath, "pnpm-lock.yaml")) {
		return InstallInfo{
			Command:        "pnpm install",
			PackageManager: "pnpm",
			LockFile:       "pnpm-lock.yaml",
			DepsDir:        "node_modules",
		}
	}
	if fileExists(filepath.Join(projectPath, "yarn.lock")) {
		return InstallInfo{
			Command:        "yarn install",
			PackageManager: "yarn",
			LockFile:       "yarn.lock",
			DepsDir:        "node_modules",
		}
	}
	// Default to npm
	return InstallInfo{
		Command:        "npm install",
		PackageManager: "npm",
		LockFile:       "package-lock.json",
		DepsDir:        "node_modules",
	}
}

func detectPythonInstallCommand(projectPath string) InstallInfo {
	// Check for various Python package managers
	if fileExists(filepath.Join(projectPath, "uv.lock")) {
		return InstallInfo{
			Command:        "uv sync",
			PackageManager: "uv",
			LockFile:       "uv.lock",
			DepsDir:        ".venv",
		}
	}
	if fileExists(filepath.Join(projectPath, "poetry.lock")) {
		return InstallInfo{
			Command:        "poetry install",
			PackageManager: "poetry",
			LockFile:       "poetry.lock",
			DepsDir:        ".venv",
		}
	}
	if fileExists(filepath.Join(projectPath, "Pipfile.lock")) {
		return InstallInfo{
			Command:        "pipenv install",
			PackageManager: "pipenv",
			LockFile:       "Pipfile.lock",
			DepsDir:        "", // Uses virtualenv
		}
	}
	if fileExists(filepath.Join(projectPath, "requirements.txt")) {
		return InstallInfo{
			Command:        "pip install -r requirements.txt",
			PackageManager: "pip",
			LockFile:       "requirements.txt",
			DepsDir:        "", // Uses site-packages
		}
	}
	// pyproject.toml with no lock file - use pip
	return InstallInfo{
		Command:        "pip install -e .",
		PackageManager: "pip",
		LockFile:       "",
		DepsDir:        "",
	}
}

func detectRubyInstallCommand(_ string) InstallInfo {
	// Ruby/Bundler always uses the same command
	return InstallInfo{
		Command:        "bundle install",
		PackageManager: "bundler",
		LockFile:       "Gemfile.lock",
		DepsDir:        "vendor/bundle",
	}
}

func detectJavaInstallCommand(projectPath string) InstallInfo {
	// Check for Gradle first (more common in modern projects)
	if fileExists(filepath.Join(projectPath, "gradlew")) {
		return InstallInfo{
			Command:        "./gradlew build",
			PackageManager: "gradle",
			LockFile:       "gradle.lockfile",
			DepsDir:        ".gradle",
		}
	}
	if fileExists(filepath.Join(projectPath, "build.gradle")) || fileExists(filepath.Join(projectPath, "build.gradle.kts")) {
		return InstallInfo{
			Command:        "gradle build",
			PackageManager: "gradle",
			LockFile:       "gradle.lockfile",
			DepsDir:        ".gradle",
		}
	}
	// Default to Maven
	return InstallInfo{
		Command:        "mvn install",
		PackageManager: "maven",
		LockFile:       "",
		DepsDir:        "", // Uses ~/.m2
	}
}

// CheckDependenciesInstalled checks if dependencies are installed for a project.
// Returns an error with the install command if dependencies are missing.
func CheckDependenciesInstalled(projectPath string, ecosystem Ecosystem) error {
	installInfo := GetInstallCommand(projectPath, ecosystem)

	// If no deps dir, we can't easily check - assume installed
	if installInfo.DepsDir == "" {
		return nil
	}

	depsPath := filepath.Join(projectPath, installInfo.DepsDir)
	if !dirExists(depsPath) {
		return fmt.Errorf("dependencies not installed - run '%s' first", installInfo.Command)
	}

	return nil
}

// Helper functions

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func containsGem(gemfileContent, gemName string) bool {
	return strings.Contains(gemfileContent, "'"+gemName+"'") ||
		strings.Contains(gemfileContent, "\""+gemName+"\"")
}
