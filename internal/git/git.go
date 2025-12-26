package git

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Repository represents a git repository
type Repository struct {
	path string
}

// NewRepository creates a new git repository handler
func NewRepository(path string) *Repository {
	return &Repository{path: path}
}

// DetectChangedFiles returns a list of files that have changed (modified or untracked)
func (r *Repository) DetectChangedFiles() ([]string, error) {
	filesMap := make(map[string]bool)

	// Get modified files (git diff)
	diffCmd := exec.Command("git", "diff", "--name-only", "HEAD")
	diffCmd.Dir = r.path
	diffOutput, err := diffCmd.Output()
	if err != nil {
		// This can fail if there's no HEAD yet (new repo with no commits)
		// Log but continue to check for untracked files
		log.Printf("git diff warning (may be expected for new repos): %v", err)
	}

	for _, line := range strings.Split(string(diffOutput), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			filesMap[line] = true
		}
	}

	// Get untracked files (git ls-files --others --exclude-standard)
	untrackedCmd := exec.Command("git", "ls-files", "--others", "--exclude-standard")
	untrackedCmd.Dir = r.path
	untrackedOutput, err := untrackedCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list untracked files: %w", err)
	}

	for _, line := range strings.Split(string(untrackedOutput), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			filesMap[line] = true
		}
	}

	// Convert map to slice
	files := []string{}
	for file := range filesMap {
		files = append(files, file)
	}

	return files, nil
}

// GenerateDiff generates a diff for a specific file
func (r *Repository) GenerateDiff(filePath string) (string, error) {
	log.Printf("🔍 GenerateDiff called for: %s", filePath)

	// First check if file is tracked by git
	lsFilesCmd := exec.Command("git", "ls-files", filePath)
	lsFilesCmd.Dir = r.path
	lsFilesOutput, err := lsFilesCmd.Output()
	if err != nil {
		log.Printf("  ⚠️  ls-files error (continuing): %v", err)
	}
	isTracked := len(bytes.TrimSpace(lsFilesOutput)) > 0
	log.Printf("  📋 Is tracked: %v", isTracked)

	if !isTracked {
		// File is untracked - check if it exists on disk
		statusCmd := exec.Command("git", "ls-files", "--others", "--exclude-standard", filePath)
		statusCmd.Dir = r.path
		statusOutput, err := statusCmd.Output()
		if err != nil {
			log.Printf("  ⚠️  ls-files --others error (continuing): %v", err)
		}

		if strings.TrimSpace(string(statusOutput)) != "" {
			log.Printf("  🆕 File is new/untracked - generating new file diff")
			// File exists but is untracked - generate a "new file" diff
			// Use platform-appropriate null device
			devNull := "/dev/null"
			if runtime.GOOS == "windows" {
				devNull = "NUL"
			}
			diffCmd := exec.Command("git", "diff", "--no-index", "--", devNull, filePath)
			diffCmd.Dir = r.path
			// git diff --no-index exits with code 1 when files differ, which is expected
			// So we intentionally ignore the error here
			diffOutput, _ := diffCmd.Output()

			log.Printf("  ✅ Generated diff length: %d bytes", len(diffOutput))
			return string(diffOutput), nil
		}

		log.Printf("  ⚠️  File not tracked and not found in untracked list")
		return "", nil
	}

	// File is tracked - try to get diff
	// Use 'git diff' without HEAD to show unstaged changes
	cmd := exec.Command("git", "diff", filePath)
	cmd.Dir = r.path
	output, err := cmd.Output()
	if err != nil {
		log.Printf("  ⚠️  git diff error (trying HEAD): %v", err)
	}

	log.Printf("  📊 Diff from 'git diff': %d bytes", len(output))

	// If still empty, try diff against HEAD (for staged changes)
	if len(output) == 0 {
		headCmd := exec.Command("git", "diff", "HEAD", filePath)
		headCmd.Dir = r.path
		headOutput, err := headCmd.Output()
		if err != nil {
			log.Printf("  ⚠️  git diff HEAD error: %v", err)
		}
		log.Printf("  📊 Diff from 'git diff HEAD': %d bytes", len(headOutput))
		output = headOutput
	}

	if len(output) == 0 {
		log.Printf("  ⚠️  No diff found for tracked file (may be unchanged)")
	}

	return string(output), nil
}

// GenerateAllDiffs generates diffs for all changed files
func (r *Repository) GenerateAllDiffs() (map[string]string, error) {
	files, err := r.DetectChangedFiles()
	if err != nil {
		return nil, err
	}

	diffs := make(map[string]string)

	for _, file := range files {
		diff, err := r.GenerateDiff(file)
		if err != nil {
			return nil, err
		}
		diffs[file] = diff
	}

	return diffs, nil
}

// Commit creates a git commit with the given message
func (r *Repository) Commit(message string) error {
	// Add all changes
	addCmd := exec.Command("git", "add", "-A")
	addCmd.Dir = r.path
	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("failed to add files: %w", err)
	}

	// Commit
	commitCmd := exec.Command("git", "commit", "-m", message)
	commitCmd.Dir = r.path

	var stderr bytes.Buffer
	commitCmd.Stderr = &stderr

	if err := commitCmd.Run(); err != nil {
		return fmt.Errorf("failed to commit: %s", stderr.String())
	}

	return nil
}

// Push pushes commits to remote
func (r *Repository) Push() error {
	cmd := exec.Command("git", "push")
	cmd.Dir = r.path

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to push: %s", stderr.String())
	}

	return nil
}

// CommitAndPush commits changes and pushes to remote (if configured)
func (r *Repository) CommitAndPush(message string) error {
	if err := r.Commit(message); err != nil {
		return err
	}

	// Try to push, but don't fail if no remote is configured
	if err := r.Push(); err != nil {
		// Check if it's a "no remote" error
		if strings.Contains(err.Error(), "No configured push destination") ||
			strings.Contains(err.Error(), "no upstream branch") {
			// No remote configured - that's OK, just committed locally
			log.Printf("ℹ️  No remote repository configured - changes committed locally only")
			return nil
		}
		// Other push errors are real errors
		return err
	}

	log.Printf("✅ Changes pushed to remote repository")
	return nil
}

// GetCurrentBranch returns the current git branch name
func (r *Repository) GetCurrentBranch() (string, error) {
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = r.path

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get branch: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

// HasChanges returns true if there are uncommitted changes
func (r *Repository) HasChanges() (bool, error) {
	files, err := r.DetectChangedFiles()
	if err != nil {
		return false, err
	}

	return len(files) > 0, nil
}

// DiscardChanges discards all uncommitted changes
func (r *Repository) DiscardChanges() error {
	// Reset all changes (git reset --hard)
	resetCmd := exec.Command("git", "reset", "--hard")
	resetCmd.Dir = r.path

	var stderr bytes.Buffer
	resetCmd.Stderr = &stderr

	if err := resetCmd.Run(); err != nil {
		return fmt.Errorf("failed to reset changes: %s", stderr.String())
	}

	// Clean untracked files (git clean -fd)
	cleanCmd := exec.Command("git", "clean", "-fd")
	cleanCmd.Dir = r.path

	cleanCmd.Stderr = &stderr

	if err := cleanCmd.Run(); err != nil {
		return fmt.Errorf("failed to clean untracked files: %s", stderr.String())
	}

	return nil
}

// CommitFile commits a single file
func (r *Repository) CommitFile(filePath string, message string) error {
	// Add the specific file
	addCmd := exec.Command("git", "add", filePath)
	addCmd.Dir = r.path
	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("failed to add file: %w", err)
	}

	// Commit the file
	commitCmd := exec.Command("git", "commit", "-m", message)
	commitCmd.Dir = r.path

	var stderr bytes.Buffer
	commitCmd.Stderr = &stderr

	if err := commitCmd.Run(); err != nil {
		return fmt.Errorf("failed to commit file: %s", stderr.String())
	}

	return nil
}

// DiscardFile discards changes to a specific file
func (r *Repository) DiscardFile(filePath string) error {
	// Check if file is tracked
	lsFilesCmd := exec.Command("git", "ls-files", filePath)
	lsFilesCmd.Dir = r.path
	lsFilesOutput, err := lsFilesCmd.Output()
	if err != nil {
		log.Printf("ls-files check warning: %v", err)
	}
	isTracked := len(bytes.TrimSpace(lsFilesOutput)) > 0

	if isTracked {
		// File is tracked - restore from HEAD
		checkoutCmd := exec.Command("git", "checkout", "HEAD", "--", filePath)
		checkoutCmd.Dir = r.path

		var stderr bytes.Buffer
		checkoutCmd.Stderr = &stderr

		if err := checkoutCmd.Run(); err != nil {
			return fmt.Errorf("failed to discard changes: %s", stderr.String())
		}
	} else {
		// File is untracked - just delete it
		cleanCmd := exec.Command("git", "clean", "-fd", filePath)
		cleanCmd.Dir = r.path

		var stderr bytes.Buffer
		cleanCmd.Stderr = &stderr

		if err := cleanCmd.Run(); err != nil {
			return fmt.Errorf("failed to remove untracked file: %s", stderr.String())
		}
	}

	return nil
}

// CommitInfo represents metadata about a git commit
type CommitInfo struct {
	Hash        string `json:"hash"`          // Short hash (7 chars)
	FullHash    string `json:"full_hash"`     // Full commit hash
	Message     string `json:"message"`       // First line of commit message
	FullMessage string `json:"full_message"`  // Complete commit message
	Author      string `json:"author"`        // Author name
	Email       string `json:"email"`         // Author email
	Timestamp   int64  `json:"timestamp"`     // Unix timestamp
	Stats       struct {
		Additions    int `json:"additions"`
		Deletions    int `json:"deletions"`
		FilesChanged int `json:"files_changed"`
	} `json:"stats"`
}

// FileChange represents changes to a single file in a commit
type FileChange struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Diff      string `json:"diff"`
}

// CommitDetails extends CommitInfo with file-level details
type CommitDetails struct {
	CommitInfo
	Files []FileChange `json:"files"`
}

// GetCommits retrieves recent git commits
func (r *Repository) GetCommits(limit int) ([]CommitInfo, error) {
	// Format: hash|full_hash|subject|body|author name|author email|unix timestamp
	format := "%h|%H|%s|%b|%an|%ae|%at"

	// Use --no-merges to avoid duplicate commits from merges
	cmd := exec.Command("git", "log", fmt.Sprintf("-%d", limit), fmt.Sprintf("--format=%s", format), "--shortstat", "--no-merges")
	cmd.Dir = r.path

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get git log: %w", err)
	}

	commits, err := parseGitLog(string(output))
	if err != nil {
		return nil, err
	}

	// Deduplicate commits by full hash (in case git returns duplicates)
	seen := make(map[string]bool)
	uniqueCommits := make([]CommitInfo, 0, len(commits))
	for _, commit := range commits {
		if !seen[commit.FullHash] {
			seen[commit.FullHash] = true
			uniqueCommits = append(uniqueCommits, commit)
		}
	}

	return uniqueCommits, nil
}

// GetCommitDetails retrieves detailed information about a specific commit
func (r *Repository) GetCommitDetails(hash string) (*CommitDetails, error) {
	// Get basic commit info
	format := "%h|%H|%s|%b|%an|%ae|%at"
	cmd := exec.Command("git", "show", hash, fmt.Sprintf("--format=%s", format), "--shortstat", "--no-patch")
	cmd.Dir = r.path

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get commit info: %w", err)
	}

	commits, err := parseGitLog(string(output))
	if err != nil || len(commits) == 0 {
		return nil, fmt.Errorf("failed to parse commit info: %w", err)
	}

	details := &CommitDetails{
		CommitInfo: commits[0],
		Files:      []FileChange{},
	}

	// Get list of changed files with stats
	filesCmd := exec.Command("git", "show", hash, "--numstat", "--format=")
	filesCmd.Dir = r.path

	filesOutput, err := filesCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get file stats: %w", err)
	}

	// Parse file stats
	for _, line := range strings.Split(string(filesOutput), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}

		additions := 0
		deletions := 0

		// Parse additions and deletions (may be "-" for binary files)
		if parts[0] != "-" {
			fmt.Sscanf(parts[0], "%d", &additions)
		}
		if parts[1] != "-" {
			fmt.Sscanf(parts[1], "%d", &deletions)
		}

		filePath := parts[2]

		// Get diff for this file
		diffCmd := exec.Command("git", "show", hash, "--", filePath)
		diffCmd.Dir = r.path
		diffOutput, err := diffCmd.Output()
		if err != nil {
			log.Printf("Warning: failed to get diff for %s: %v", filePath, err)
			// Continue without diff content
		}

		details.Files = append(details.Files, FileChange{
			Path:      filePath,
			Additions: additions,
			Deletions: deletions,
			Diff:      string(diffOutput),
		})
	}

	return details, nil
}

// GetLatestCommit retrieves the most recent commit
func (r *Repository) GetLatestCommit() (*CommitInfo, error) {
	commits, err := r.GetCommits(1)
	if err != nil {
		return nil, err
	}

	if len(commits) == 0 {
		return nil, fmt.Errorf("no commits found")
	}

	return &commits[0], nil
}

// parseGitLog parses git log output into CommitInfo structs
func parseGitLog(output string) ([]CommitInfo, error) {
	var commits []CommitInfo
	lines := strings.Split(output, "\n")

	var currentCommit *CommitInfo
	var bodyLines []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Check if this is a commit info line (contains pipes)
		if strings.Contains(line, "|") {
			// Save previous commit if exists
			if currentCommit != nil {
				if len(bodyLines) > 0 {
					currentCommit.FullMessage = strings.Join(bodyLines, "\n")
				}
				commits = append(commits, *currentCommit)
			}

			// Parse new commit line
			parts := strings.Split(line, "|")
			if len(parts) < 7 {
				continue
			}

			var timestamp int64
			fmt.Sscanf(parts[6], "%d", &timestamp)

			currentCommit = &CommitInfo{
				Hash:        parts[0],
				FullHash:    parts[1],
				Message:     parts[2],
				FullMessage: parts[2], // Will be updated with body if present
				Author:      parts[4],
				Email:       parts[5],
				Timestamp:   timestamp,
			}

			bodyLines = []string{parts[2]} // Start with subject
			if parts[3] != "" {
				bodyLines = append(bodyLines, "", parts[3]) // Add body if present
			}

		} else if strings.Contains(line, "changed,") || strings.Contains(line, "insertion") || strings.Contains(line, "deletion") {
			// Parse shortstat line: "3 files changed, 45 insertions(+), 12 deletions(-)"
			if currentCommit != nil {
				fields := strings.Fields(line)
				for i, field := range fields {
					if field == "changed," && i > 0 {
						fmt.Sscanf(fields[i-1], "%d", &currentCommit.Stats.FilesChanged)
					}
					if strings.HasPrefix(field, "insertion") && i > 0 {
						fmt.Sscanf(fields[i-1], "%d", &currentCommit.Stats.Additions)
					}
					if strings.HasPrefix(field, "deletion") && i > 0 {
						fmt.Sscanf(fields[i-1], "%d", &currentCommit.Stats.Deletions)
					}
				}
			}
		}
	}

	// Don't forget the last commit
	if currentCommit != nil {
		if len(bodyLines) > 0 {
			currentCommit.FullMessage = strings.Join(bodyLines, "\n")
		}
		commits = append(commits, *currentCommit)
	}

	return commits, nil
}

// EnsureGitRepo ensures the path is a git repository.
// If not, initializes one with a .gitignore.
func EnsureGitRepo(path string) error {
	gitDir := filepath.Join(path, ".git")

	if _, err := os.Stat(gitDir); err == nil {
		// Already a git repo
		return nil
	}

	// Initialize git
	cmd := exec.Command("git", "init")
	cmd.Dir = path
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to init git: %w", err)
	}

	// Create basic .gitignore if it doesn't exist
	gitignorePath := filepath.Join(path, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		defaultIgnore := `# Dependencies
node_modules/
__pycache__/
*.pyc
.venv/
venv/

# Build outputs
dist/
build/
*.egg-info/

# IDE
.idea/
.vscode/
*.swp
*.swo

# OS
.DS_Store
Thumbs.db

# Environment
.env
.env.local
*.log
`
		if err := os.WriteFile(gitignorePath, []byte(defaultIgnore), 0644); err != nil {
			log.Printf("Warning: failed to write .gitignore: %v", err)
			// Continue anyway - .gitignore is nice to have but not critical
		}
	}

	// Initial commit
	addCmd := exec.Command("git", "add", "-A")
	addCmd.Dir = path
	if err := addCmd.Run(); err != nil {
		log.Printf("Warning: failed to stage files for initial commit: %v", err)
		// Continue - might have nothing to stage
	}

	commitCmd := exec.Command("git", "commit", "-m", "Initial commit (Finn)")
	commitCmd.Dir = path
	if err := commitCmd.Run(); err != nil {
		log.Printf("Warning: failed to create initial commit: %v", err)
		// This is expected if there's nothing to commit
	}

	return nil
}

// IsGitRepo checks if a path is a git repository
func IsGitRepo(path string) bool {
	gitDir := filepath.Join(path, ".git")
	_, err := os.Stat(gitDir)
	return err == nil
}

// GetHeadHash returns the current HEAD commit hash (full hash)
// This is a lightweight operation that just reads the ref
func (r *Repository) GetHeadHash() (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = r.path

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get HEAD hash: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

// GetCommitsSince retrieves commits since a given hash (exclusive)
// Returns commits in reverse chronological order (newest first)
// If sinceHash is empty, returns the last `limit` commits
func (r *Repository) GetCommitsSince(sinceHash string, limit int) ([]CommitInfo, error) {
	var cmd *exec.Cmd

	if sinceHash == "" {
		// No reference hash, just get recent commits
		cmd = exec.Command("git", "log", fmt.Sprintf("-%d", limit), "--format=%h|%H|%s|%b|%an|%ae|%at", "--shortstat", "--no-merges")
	} else {
		// Get commits since the reference hash (exclusive of sinceHash)
		// Use sinceHash..HEAD to get commits reachable from HEAD but not from sinceHash
		cmd = exec.Command("git", "log", fmt.Sprintf("%s..HEAD", sinceHash), "--format=%h|%H|%s|%b|%an|%ae|%at", "--shortstat", "--no-merges")
	}

	cmd.Dir = r.path

	output, err := cmd.Output()
	if err != nil {
		// If the sinceHash doesn't exist (e.g., after a reset), fall back to getting recent commits
		if sinceHash != "" {
			return r.GetCommits(limit)
		}
		return nil, fmt.Errorf("failed to get commits since %s: %w", sinceHash, err)
	}

	commits, err := parseGitLog(string(output))
	if err != nil {
		return nil, err
	}

	// Deduplicate by full hash
	seen := make(map[string]bool)
	uniqueCommits := make([]CommitInfo, 0, len(commits))
	for _, commit := range commits {
		if !seen[commit.FullHash] {
			seen[commit.FullHash] = true
			uniqueCommits = append(uniqueCommits, commit)
		}
	}

	// Limit results if we got more than requested
	if limit > 0 && len(uniqueCommits) > limit {
		uniqueCommits = uniqueCommits[:limit]
	}

	return uniqueCommits, nil
}

