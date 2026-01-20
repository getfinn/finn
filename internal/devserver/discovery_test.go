package devserver

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoverProjects(t *testing.T) {
	// Create temp directory structure for testing
	tmpDir, err := os.MkdirTemp("", "discovery-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test project structure:
	// tmpDir/
	//   frontend/package.json (Next.js)
	//   backend/go.mod
	//   services/api/requirements.txt (Flask)
	//   node_modules/... (should be skipped)

	// Frontend - Next.js
	frontendDir := filepath.Join(tmpDir, "frontend")
	os.MkdirAll(frontendDir, 0755)
	os.WriteFile(filepath.Join(frontendDir, "package.json"), []byte(`{
		"name": "frontend",
		"dependencies": { "next": "14.0.0" },
		"scripts": { "dev": "next dev" }
	}`), 0644)

	// Backend - Go
	backendDir := filepath.Join(tmpDir, "backend")
	os.MkdirAll(backendDir, 0755)
	os.WriteFile(filepath.Join(backendDir, "go.mod"), []byte(`module github.com/test/backend
go 1.21`), 0644)
	os.WriteFile(filepath.Join(backendDir, "main.go"), []byte(`package main
func main() {}`), 0644)

	// API service - Python Flask
	apiDir := filepath.Join(tmpDir, "services", "api")
	os.MkdirAll(apiDir, 0755)
	os.WriteFile(filepath.Join(apiDir, "requirements.txt"), []byte(`flask==2.0.0`), 0644)

	// node_modules - should be skipped
	nodeModulesDir := filepath.Join(tmpDir, "node_modules", "some-package")
	os.MkdirAll(nodeModulesDir, 0755)
	os.WriteFile(filepath.Join(nodeModulesDir, "package.json"), []byte(`{"name":"some-package"}`), 0644)

	// Run discovery
	projects, err := DiscoverProjects(tmpDir, 3)
	if err != nil {
		t.Fatalf("DiscoverProjects failed: %v", err)
	}

	// Should find 3 projects (frontend, backend, api)
	if len(projects) != 3 {
		t.Errorf("Expected 3 projects, got %d", len(projects))
		for _, p := range projects {
			t.Logf("Found: %s (%s)", p.Name, p.RelPath)
		}
	}

	// Check each project
	projectsByName := make(map[string]DiscoveredProject)
	for _, p := range projects {
		projectsByName[p.Name] = p
	}

	// Frontend should be Next.js
	if frontend, ok := projectsByName["frontend"]; ok {
		if frontend.Ecosystem != EcosystemNode {
			t.Errorf("Frontend ecosystem: got %s, want %s", frontend.Ecosystem, EcosystemNode)
		}
		if frontend.Framework != "nextjs" {
			t.Errorf("Frontend framework: got %s, want nextjs", frontend.Framework)
		}
		if !frontend.HasDevCmd {
			t.Error("Frontend should have dev command")
		}
	} else {
		t.Error("Frontend project not found")
	}

	// Backend should be Go
	if backend, ok := projectsByName["backend"]; ok {
		if backend.Ecosystem != EcosystemGo {
			t.Errorf("Backend ecosystem: got %s, want %s", backend.Ecosystem, EcosystemGo)
		}
		if !backend.HasDevCmd {
			t.Error("Backend should have dev command (main.go present)")
		}
	} else {
		t.Error("Backend project not found")
	}

	// API should be Python/Flask
	if api, ok := projectsByName["api"]; ok {
		if api.Ecosystem != EcosystemPython {
			t.Errorf("API ecosystem: got %s, want %s", api.Ecosystem, EcosystemPython)
		}
		if api.Framework != "flask" {
			t.Errorf("API framework: got %s, want flask", api.Framework)
		}
	} else {
		t.Error("API project not found")
	}
}

func TestDiscoverProjects_MaxDepth(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "depth-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create deeply nested project
	deepDir := filepath.Join(tmpDir, "a", "b", "c", "d", "project")
	os.MkdirAll(deepDir, 0755)
	os.WriteFile(filepath.Join(deepDir, "package.json"), []byte(`{"name":"deep"}`), 0644)

	// Shallow project
	shallowDir := filepath.Join(tmpDir, "shallow")
	os.MkdirAll(shallowDir, 0755)
	os.WriteFile(filepath.Join(shallowDir, "package.json"), []byte(`{"name":"shallow"}`), 0644)

	// With depth 2, should only find shallow
	projects, _ := DiscoverProjects(tmpDir, 2)
	if len(projects) != 1 {
		t.Errorf("With depth 2: expected 1 project, got %d", len(projects))
	}

	// With depth 5, should find both
	projects, _ = DiscoverProjects(tmpDir, 5)
	if len(projects) != 2 {
		t.Errorf("With depth 5: expected 2 projects, got %d", len(projects))
	}
}

func TestSelectProjectForPreview_SingleProject(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "select-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create single Next.js project
	os.WriteFile(filepath.Join(tmpDir, "package.json"), []byte(`{
		"name": "single-app",
		"dependencies": { "next": "14.0.0" },
		"scripts": { "dev": "next dev" }
	}`), 0644)

	result := SelectProjectForPreview(tmpDir, "folder-123", "", nil, nil)

	if result.NeedsUserInput {
		t.Error("Single project should not need user input")
	}
	if result.Selected == nil {
		t.Fatal("Should have selected a project")
	}
	if result.SelectionReason != "single_project" {
		t.Errorf("Selection reason: got %s, want single_project", result.SelectionReason)
	}
}

func TestSelectProjectForPreview_MultipleProjects(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "multi-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create two projects
	app1 := filepath.Join(tmpDir, "app1")
	os.MkdirAll(app1, 0755)
	os.WriteFile(filepath.Join(app1, "package.json"), []byte(`{
		"name": "app1",
		"dependencies": { "next": "14.0.0" },
		"scripts": { "dev": "next dev" }
	}`), 0644)

	app2 := filepath.Join(tmpDir, "app2")
	os.MkdirAll(app2, 0755)
	os.WriteFile(filepath.Join(app2, "package.json"), []byte(`{
		"name": "app2",
		"dependencies": { "vite": "5.0.0" },
		"scripts": { "dev": "vite" }
	}`), 0644)

	result := SelectProjectForPreview(tmpDir, "folder-123", "", nil, nil)

	if !result.NeedsUserInput {
		t.Error("Multiple projects should need user input")
	}
	if result.PreviewableCount != 2 {
		t.Errorf("PreviewableCount: got %d, want 2", result.PreviewableCount)
	}
	if result.SelectionReason != "multiple_projects" {
		t.Errorf("Selection reason: got %s, want multiple_projects", result.SelectionReason)
	}
}

func TestSelectProjectForPreview_MultipleProjects_AlwaysAskUser(t *testing.T) {
	// For monorepos with multiple projects, we always show the picker
	// even if there's a cached selection - user should choose each time
	tmpDir, err := os.MkdirTemp("", "multi-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create two projects
	app1 := filepath.Join(tmpDir, "app1")
	os.MkdirAll(app1, 0755)
	os.WriteFile(filepath.Join(app1, "package.json"), []byte(`{
		"name": "app1",
		"dependencies": { "next": "14.0.0" },
		"scripts": { "dev": "next dev" }
	}`), 0644)

	app2 := filepath.Join(tmpDir, "app2")
	os.MkdirAll(app2, 0755)
	os.WriteFile(filepath.Join(app2, "package.json"), []byte(`{
		"name": "app2",
		"dependencies": { "vite": "5.0.0" },
		"scripts": { "dev": "vite" }
	}`), 0644)

	// Set up cache to prefer app2
	cache := NewPreviewConfigCache()
	cache.Set("folder-123", PreviewConfig{
		ProjectPath: "app2",
		DevCommand:  "npm run dev",
	})

	// Even with cache, multiple projects should require user input
	result := SelectProjectForPreview(tmpDir, "folder-123", "", nil, cache)

	if !result.NeedsUserInput {
		t.Error("Multiple projects should always need user input (even with cache)")
	}
	if result.PreviewableCount != 2 {
		t.Errorf("PreviewableCount: got %d, want 2", result.PreviewableCount)
	}
	if result.SelectionReason != "multiple_projects" {
		t.Errorf("Selection reason: got %s, want multiple_projects", result.SelectionReason)
	}
}

func TestActivityTracker(t *testing.T) {
	tracker := NewActivityTracker()

	// Record some activity
	tracker.RecordActivity("folder-1", "/path/to/project/src/main.ts")
	tracker.RecordActivity("folder-1", "/path/to/project/src/utils.ts")
	tracker.RecordActivity("folder-1", "/path/to/other/file.ts")

	// Get recent files
	files := tracker.GetRecentFiles("folder-1", 10)
	if len(files) != 3 {
		t.Errorf("Expected 3 recent files, got %d", len(files))
	}

	// Most recent should be first
	if files[0] != "/path/to/other/file.ts" {
		t.Errorf("Expected most recent file first, got %s", files[0])
	}

	// Test clear
	tracker.Clear("folder-1")
	files = tracker.GetRecentFiles("folder-1", 10)
	if len(files) != 0 {
		t.Error("Expected no files after clear")
	}
}

func TestActivityTracker_StaleFolderCleanup(t *testing.T) {
	tracker := &ActivityTracker{
		activities:      make(map[string][]FileActivity),
		maxAge:          100 * time.Millisecond, // Short for testing
		maxPerFolder:    100,
		cleanupInterval: 50 * time.Millisecond, // Short for testing
	}

	// Record activity in two folders
	tracker.RecordActivity("folder-1", "/path/to/file1.ts")
	tracker.RecordActivity("folder-2", "/path/to/file2.ts")

	// Both folders should exist
	tracker.mu.RLock()
	if len(tracker.activities) != 2 {
		t.Errorf("Expected 2 folders, got %d", len(tracker.activities))
	}
	tracker.mu.RUnlock()

	// Wait for activities to become stale
	time.Sleep(150 * time.Millisecond)

	// Record new activity in folder-1 only (triggers cleanup)
	tracker.RecordActivity("folder-1", "/path/to/file3.ts")

	// folder-2 should be cleaned up (all its activities are stale)
	// folder-1 should still exist (has fresh activity)
	tracker.mu.RLock()
	if _, exists := tracker.activities["folder-1"]; !exists {
		t.Error("folder-1 should still exist")
	}
	if _, exists := tracker.activities["folder-2"]; exists {
		t.Error("folder-2 should have been cleaned up")
	}
	tracker.mu.RUnlock()
}

func TestGetInstallCommand_NodeEcosystem(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "install-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tests := []struct {
		name           string
		lockFile       string
		wantPM         string
		wantCommand    string
	}{
		{"npm (package-lock)", "package-lock.json", "npm", "npm install"},
		{"yarn (yarn.lock)", "yarn.lock", "yarn", "yarn install"},
		{"pnpm (pnpm-lock)", "pnpm-lock.yaml", "pnpm", "pnpm install"},
		{"bun (bun.lockb)", "bun.lockb", "bun", "bun install"},
		{"default to npm", "", "npm", "npm install"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up any previous lock files
			os.Remove(filepath.Join(tmpDir, "package-lock.json"))
			os.Remove(filepath.Join(tmpDir, "yarn.lock"))
			os.Remove(filepath.Join(tmpDir, "pnpm-lock.yaml"))
			os.Remove(filepath.Join(tmpDir, "bun.lockb"))

			// Create the lock file if specified
			if tt.lockFile != "" {
				os.WriteFile(filepath.Join(tmpDir, tt.lockFile), []byte(""), 0644)
			}

			info := GetInstallCommand(tmpDir, EcosystemNode)

			if info.PackageManager != tt.wantPM {
				t.Errorf("PackageManager: got %s, want %s", info.PackageManager, tt.wantPM)
			}
			if info.Command != tt.wantCommand {
				t.Errorf("Command: got %s, want %s", info.Command, tt.wantCommand)
			}
			if info.DepsDir != "node_modules" {
				t.Errorf("DepsDir: got %s, want node_modules", info.DepsDir)
			}
		})
	}
}

func TestGetInstallCommand_PythonEcosystem(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "install-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tests := []struct {
		name        string
		setupFiles  map[string]string
		wantPM      string
		wantCommand string
	}{
		{
			"poetry",
			map[string]string{"poetry.lock": ""},
			"poetry",
			"poetry install",
		},
		{
			"pipenv",
			map[string]string{"Pipfile.lock": ""},
			"pipenv",
			"pipenv install",
		},
		{
			"pip requirements",
			map[string]string{"requirements.txt": "flask==2.0.0"},
			"pip",
			"pip install -r requirements.txt",
		},
		{
			"default pip (editable install)",
			map[string]string{},
			"pip",
			"pip install -e .",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up
			os.Remove(filepath.Join(tmpDir, "poetry.lock"))
			os.Remove(filepath.Join(tmpDir, "Pipfile.lock"))
			os.Remove(filepath.Join(tmpDir, "requirements.txt"))

			// Create files
			for name, content := range tt.setupFiles {
				os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0644)
			}

			info := GetInstallCommand(tmpDir, EcosystemPython)

			if info.PackageManager != tt.wantPM {
				t.Errorf("PackageManager: got %s, want %s", info.PackageManager, tt.wantPM)
			}
			if info.Command != tt.wantCommand {
				t.Errorf("Command: got %s, want %s", info.Command, tt.wantCommand)
			}
		})
	}
}

func TestGetInstallCommand_JavaEcosystem(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "install-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Test Gradle with wrapper
	os.WriteFile(filepath.Join(tmpDir, "gradlew"), []byte("#!/bin/bash"), 0755)
	info := GetInstallCommand(tmpDir, EcosystemJava)
	if info.PackageManager != "gradle" {
		t.Errorf("Expected gradle, got %s", info.PackageManager)
	}
	if info.Command != "./gradlew build" {
		t.Errorf("Expected './gradlew build', got %s", info.Command)
	}
	os.Remove(filepath.Join(tmpDir, "gradlew"))

	// Test Maven
	os.WriteFile(filepath.Join(tmpDir, "pom.xml"), []byte("<project></project>"), 0644)
	info = GetInstallCommand(tmpDir, EcosystemJava)
	if info.PackageManager != "maven" {
		t.Errorf("Expected maven, got %s", info.PackageManager)
	}
	if info.Command != "mvn install" {
		t.Errorf("Expected 'mvn install', got %s", info.Command)
	}
}

func TestGetInstallCommand_OtherEcosystems(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "install-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tests := []struct {
		ecosystem   Ecosystem
		wantPM      string
		wantCommand string
	}{
		{EcosystemGo, "go", "go mod download"},
		{EcosystemRust, "cargo", "cargo build"},
		{EcosystemRuby, "bundler", "bundle install"},
		{EcosystemPHP, "composer", "composer install"},
		{EcosystemElixir, "mix", "mix deps.get"},
		{EcosystemDotNet, "dotnet", "dotnet restore"},
	}

	for _, tt := range tests {
		t.Run(string(tt.ecosystem), func(t *testing.T) {
			info := GetInstallCommand(tmpDir, tt.ecosystem)

			if info.PackageManager != tt.wantPM {
				t.Errorf("PackageManager: got %s, want %s", info.PackageManager, tt.wantPM)
			}
			if info.Command != tt.wantCommand {
				t.Errorf("Command: got %s, want %s", info.Command, tt.wantCommand)
			}
		})
	}
}

func TestCheckDependenciesInstalled(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "deps-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Node.js without node_modules should error
	err = CheckDependenciesInstalled(tmpDir, EcosystemNode)
	if err == nil {
		t.Error("Expected error for missing node_modules")
	}

	// Create node_modules
	os.MkdirAll(filepath.Join(tmpDir, "node_modules"), 0755)

	// Now should pass
	err = CheckDependenciesInstalled(tmpDir, EcosystemNode)
	if err != nil {
		t.Errorf("Expected no error with node_modules, got: %v", err)
	}

	// Go has no DepsDir to check, should always pass
	err = CheckDependenciesInstalled(tmpDir, EcosystemGo)
	if err != nil {
		t.Errorf("Go should not require deps dir check, got: %v", err)
	}
}
