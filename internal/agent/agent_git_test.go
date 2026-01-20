package agent

import "testing"

func TestShouldSkipFile(t *testing.T) {
	tests := []struct {
		name     string
		filePath string
		want     bool
	}{
		// Should skip - build directories
		{"next build dir", "frontend/.next/app/page.js", true},
		{"next at root", ".next/static/main.js", true},
		{"nuxt build dir", ".nuxt/components.js", true},
		{"node_modules", "node_modules/react/index.js", true},
		{"nested node_modules", "packages/app/node_modules/lodash/index.js", true},
		{"dist folder", "dist/bundle.js", true},
		{"build folder", "build/output.js", true},
		{"vendor folder", "vendor/github.com/pkg/errors/errors.go", true},
		{"pycache", "__pycache__/module.pyc", true},
		{"venv", ".venv/lib/python3.9/site.py", true},
		{"git dir", ".git/objects/pack/pack-123.idx", true},

		// Should skip - binary files
		{"png image", "assets/logo.png", true},
		{"jpg image", "images/photo.JPG", true},
		{"woff font", "fonts/roboto.woff2", true},
		{"pdf file", "docs/manual.pdf", true},
		{"pack file", "cache/data.pack.gz", true},

		// Should NOT skip - regular source files
		{"tsx file", "src/App.tsx", false},
		{"go file", "main.go", false},
		{"python file", "app/views.py", false},
		{"config file", "next.config.js", false},
		{"package.json", "package.json", false},
		{"nested source", "frontend/src/components/Button.tsx", false},
		{"gitignore", ".gitignore", false},
		{"env file", ".env.local", false},
		{"buildUtils file", "src/buildUtils.js", false},
		{"file with next in name", "components/NextButton.tsx", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSkipFile(tt.filePath)
			if got != tt.want {
				t.Errorf("shouldSkipFile(%q) = %v, want %v", tt.filePath, got, tt.want)
			}
		})
	}
}
