package devserver

import (
	"reflect"
	"testing"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "simple command",
			input:    "npm run dev",
			expected: []string{"npm", "run", "dev"},
		},
		{
			name:     "double quoted argument",
			input:    `npm run "my script"`,
			expected: []string{"npm", "run", "my script"},
		},
		{
			name:     "single quoted argument",
			input:    `python -c 'print("hello")'`,
			expected: []string{"python", "-c", `print("hello")`},
		},
		{
			name:     "mixed quotes",
			input:    `echo "hello" 'world'`,
			expected: []string{"echo", "hello", "world"},
		},
		{
			name:     "path with spaces",
			input:    `"/path/to my/app" --port 3000`,
			expected: []string{"/path/to my/app", "--port", "3000"},
		},
		{
			name:     "escaped quote",
			input:    `echo "hello \"world\""`,
			expected: []string{"echo", `hello "world"`},
		},
		{
			name:     "multiple spaces",
			input:    "npm   run    dev",
			expected: []string{"npm", "run", "dev"},
		},
		{
			name:     "tabs and spaces",
			input:    "npm\trun\t dev",
			expected: []string{"npm", "run", "dev"},
		},
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "only spaces",
			input:    "   ",
			expected: nil,
		},
		{
			name:     "uvicorn command",
			input:    "uvicorn main:app --reload",
			expected: []string{"uvicorn", "main:app", "--reload"},
		},
		{
			name:     "rails server",
			input:    "rails server -p 3000",
			expected: []string{"rails", "server", "-p", "3000"},
		},
		{
			name:     "go run with path",
			input:    "go run ./cmd/server",
			expected: []string{"go", "run", "./cmd/server"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseCommand(tt.input)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("parseCommand(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestInjectPort(t *testing.T) {
	tests := []struct {
		name      string
		cmdParts  []string
		ecosystem Ecosystem
		port      int
		expected  []string
	}{
		{
			name:      "node - no injection (uses PORT env)",
			cmdParts:  []string{"npm", "run", "dev"},
			ecosystem: EcosystemNode,
			port:      3000,
			expected:  []string{"npm", "run", "dev"},
		},
		{
			name:      "python uvicorn - adds port flag",
			cmdParts:  []string{"uvicorn", "main:app", "--reload"},
			ecosystem: EcosystemPython,
			port:      8000,
			expected:  []string{"uvicorn", "main:app", "--reload", "--port", "8000"},
		},
		{
			name:      "python flask - no injection (uses PORT env)",
			cmdParts:  []string{"flask", "run"},
			ecosystem: EcosystemPython,
			port:      5000,
			expected:  []string{"flask", "run"},
		},
		{
			name:      "ruby rails server - adds port flag",
			cmdParts:  []string{"rails", "server"},
			ecosystem: EcosystemRuby,
			port:      3000,
			expected:  []string{"rails", "server", "-p", "3000"},
		},
		{
			name:      "go - no injection (uses PORT env)",
			cmdParts:  []string{"go", "run", "."},
			ecosystem: EcosystemGo,
			port:      8080,
			expected:  []string{"go", "run", "."},
		},
		{
			name:      "php artisan serve - adds port flag",
			cmdParts:  []string{"php", "artisan", "serve"},
			ecosystem: EcosystemPHP,
			port:      8000,
			expected:  []string{"php", "artisan", "serve", "--port=8000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := injectPort(tt.cmdParts, tt.ecosystem, tt.port)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("injectPort(%v, %v, %d) = %v, want %v",
					tt.cmdParts, tt.ecosystem, tt.port, result, tt.expected)
			}
		})
	}
}
